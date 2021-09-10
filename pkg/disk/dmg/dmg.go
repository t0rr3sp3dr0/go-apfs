package dmg

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/apex/log"
	"github.com/blacktop/go-apfs/pkg/adc"
	"github.com/blacktop/go-apfs/pkg/disk/gpt"
	"github.com/blacktop/go-macho/types"
	"github.com/blacktop/go-plist"

	lzfse "github.com/blacktop/lzfse-cgo"
	lru "github.com/hashicorp/golang-lru"
	"github.com/vbauerster/mpb/v7"
	"github.com/vbauerster/mpb/v7/decor"
)

const sectorSize = 0x200

// DMG apple disk image object
type DMG struct {
	Footer UDIFResourceFile
	Plist  resourceFork
	Nsiz   nsiz
	Blocks []UDIFBlockData

	firstAPFSPartition  int
	apfsPartitionOffset uint64
	apfsPartitionSize   uint64

	cache        *lru.Cache
	evictCounter uint64

	sr     *io.SectionReader
	closer io.Closer
}

type block struct {
	Attributes string
	Data       []byte
	ID         string
	Name       string
	CFName     string `plist:"CFName,omitempty"`
}

type resourceFork struct {
	ResourceFork map[string][]block `plist:"resource-fork,omitempty"`
}

type volAndUUID struct {
	Name string `plist:"name,omitempty"`
	UUID string `plist:"uuid,omitempty"`
}

type nsiz struct {
	Sha1Digest          []byte       `plist:"SHA-1-digest,omitempty"`
	Sha256Digest        []byte       `plist:"SHA-256-digest,omitempty"`
	VolumeNamesAndUUIDs []volAndUUID `plist:"Volume names and UUIDs,omitempty"`
	BlockChecksum2      int          `plist:"block-checksum-2,omitempty"`
	PartNum             int          `plist:"part-num,omitempty"`
	Version             int          `plist:"version,omitempty"`
}

type udifSignature [4]byte

func (s udifSignature) String() string {
	return string(s[:])
}

type udifChecksumType uint32

const (
	NONE_TYPE  udifChecksumType = 0
	CRC32_TYPE udifChecksumType = 2
)

// UDIFChecksum object
type UDIFChecksum struct {
	Type udifChecksumType
	Size uint32
	Data [32]uint32
}

const (
	udifRFSignature = "koly"
	udifRFVersion   = 4
	udifSectorSize  = 512
)

type udifResourceFileFlag uint32

const (
	Flattened       udifResourceFileFlag = 0x00000001
	InternetEnabled udifResourceFileFlag = 0x00000004
)

// UDIFResourceFile - Universal Disk Image Format (UDIF)
type UDIFResourceFile struct {
	Signature             udifSignature // magic 'koly'
	Version               uint32        // 4 (as of 2013)
	HeaderSize            uint32        // sizeof(this) =  512 (as of 2013)
	Flags                 udifResourceFileFlag
	RunningDataForkOffset uint64
	DataForkOffset        uint64 // usually 0, beginning of file
	DataForkLength        uint64
	RsrcForkOffset        uint64 // resource fork offset and length
	RsrcForkLength        uint64
	SegmentNumber         uint32 // Usually 1, can be 0
	SegmentCount          uint32 // Usually 1, can be 0
	SegmentID             types.UUID

	DataChecksum UDIFChecksum

	PlistOffset uint64 // Offset and length of the blkx plist.
	PlistLength uint64

	Reserved1 [64]byte

	CodeSignatureOffset uint64
	CodeSignatureLength uint64

	Reserved2 [40]byte

	MasterChecksum UDIFChecksum

	ImageVariant uint32 // Unknown, commonly 1
	SectorCount  uint64

	Reserved3 uint32
	Reserved4 uint32
	Reserved5 uint32
}

const (
	udifBDSignature = "mish"
	udifBDVersion   = 1
)

type udifBlockData struct {
	Signature   udifSignature // magic 'mish'
	Version     uint32
	StartSector uint64 // Logical block offset and length, in sectors.
	SectorCount uint64

	DataOffset       uint64
	BuffersNeeded    uint32
	BlockDescriptors uint32

	Reserved1 uint32
	Reserved2 uint32
	Reserved3 uint32
	Reserved4 uint32
	Reserved5 uint32
	Reserved6 uint32

	Checksum UDIFChecksum

	ChunkCount uint32
}

// UDIFBlockData object
type UDIFBlockData struct {
	Name string
	udifBlockData
	Chunks []udifBlockChunk

	sr *io.SectionReader
}

type udifBlockChunkType uint32

const (
	ZERO_FILL       udifBlockChunkType = 0x00000000
	UNCOMPRESSED    udifBlockChunkType = 0x00000001
	IGNORED         udifBlockChunkType = 0x00000002 // Sparse (used for Apple_Free)
	COMPRESS_ADC    udifBlockChunkType = 0x80000004
	COMPRESS_ZLIB   udifBlockChunkType = 0x80000005
	COMPRESSS_BZ2   udifBlockChunkType = 0x80000006
	COMPRESSS_LZFSE udifBlockChunkType = 0x80000007
	COMPRESSS_LZMA  udifBlockChunkType = 0x80000008
	COMMENT         udifBlockChunkType = 0x7ffffffe
	LAST_BLOCK      udifBlockChunkType = 0xffffffff
)

type udifBlockChunk struct {
	Type             udifBlockChunkType
	Comment          uint32
	DiskOffset       uint64 // Logical chunk offset and length, in sectors.
	DiskLength       uint64
	CompressedOffset uint64 // Compressed offset and length, in bytes.
	CompressedLength uint64
}

func (b *UDIFBlockData) maxChunkSize() int {
	var max int
	for _, chunk := range b.Chunks {
		if max < int(chunk.CompressedLength) {
			max = int(chunk.CompressedLength)
		}
	}
	return max
}

// DecompressChunks decompresses the chunks for a given block and writes them to supplied bufio.Writer
func (b *UDIFBlockData) DecompressChunks(w *bufio.Writer) error {
	var n int
	var total int
	var err error

	log.Infof("Decompressing DMG block %s", b.Name)

	// initialize progress bar
	p := mpb.New(mpb.WithWidth(80))
	// adding a single bar, which will inherit container's width
	bar := p.Add(int64(len(b.Chunks)),
		// progress bar filler with customized style
		mpb.NewBarFiller(mpb.BarStyle().Lbound("[").Filler("=").Tip(">").Padding("-").Rbound("|")),
		mpb.PrependDecorators(
			decor.Name("     ", decor.WC{W: len("     ") + 1, C: decor.DidentRight}),
			// replace ETA decorator with "done" message, OnComplete event
			decor.OnComplete(
				decor.AverageETA(decor.ET_STYLE_GO, decor.WC{W: 4}), "✅ ",
			),
		),
		mpb.AppendDecorators(decor.Percentage()),
	)

	buff := make([]byte, 0, b.maxChunkSize())

	// for _, chunk := range b.Chunks[:50] {
	for _, chunk := range b.Chunks {
		// TODO: verify chunk (size not greater than block etc)
		switch chunk.Type {
		case ZERO_FILL:

			n, err = w.Write(make([]byte, chunk.CompressedLength))
			if err != nil {
				return err
			}
			total += n
			log.Debugf("Wrote %#x bytes of ZERO_FILL data (output size: %#x)", n, total)
		case UNCOMPRESSED:
			buff = buff[:chunk.CompressedLength]
			_, err = b.sr.ReadAt(buff, int64(chunk.CompressedOffset))
			if err != nil {
				return err
			}

			n, err = w.Write(buff)
			if err != nil {
				return err
			}
			total += n
			log.Debugf("Wrote %#x bytes of UNCOMPRESSED data (output size: %#x)", n, total)
		case IGNORED:

			n, err = w.Write(make([]byte, chunk.DiskLength*udifSectorSize))
			if err != nil {
				return err
			}
			total += n
			log.Debugf("Wrote %#x bytes of IGNORED data (output size: %#x)", n, total)
		case COMPRESS_ADC:
			buff = buff[:chunk.CompressedLength]
			_, err = b.sr.ReadAt(buff, int64(chunk.CompressedOffset))
			if err != nil {
				return err
			}

			n, err = w.Write(adc.DecompressADC(buff))
			if err != nil {
				return err
			}
			total += n
			log.Debugf("Wrote %#x bytes of COMPRESS_ADC data (output size: %#x)", n, total)
		case COMPRESS_ZLIB:
			buff = buff[:chunk.CompressedLength]
			_, err = b.sr.ReadAt(buff, int64(chunk.CompressedOffset))
			if err != nil {
				return err
			}
			r, err := zlib.NewReader(bytes.NewReader(buff))
			if err != nil {
				return err
			}

			n, err := w.ReadFrom(r)
			if err != nil {
				return err
			}
			r.Close()
			total += int(n)
			log.Debugf("Wrote %#x bytes of COMPRESS_ZLIB data (output size: %#x)", n, total)
		case COMPRESSS_BZ2:
			buff = buff[:chunk.CompressedLength]
			if _, err := b.sr.ReadAt(buff, int64(chunk.CompressedOffset)); err != nil {
				return err
			}

			n, err := w.ReadFrom(bzip2.NewReader(bytes.NewReader(buff)))
			if err != nil {
				return err
			}
			total += int(n)
			log.Debugf("Wrote %#x bytes of COMPRESSS_BZ2 data (output size: %#x)", n, total)
		case COMPRESSS_LZFSE:
			buff = buff[:chunk.CompressedLength]
			if _, err := b.sr.ReadAt(buff, int64(chunk.CompressedOffset)); err != nil {
				return err
			}
			n, err = w.Write(lzfse.DecodeBuffer(buff))
			if err != nil {
				return err
			}
			total += n
			log.Debugf("Wrote %#x bytes of COMPRESSS_LZFSE data (output size: %#x)", n, total)
		case COMPRESSS_LZMA:
			return fmt.Errorf("COMPRESSS_LZMA is currently unsupported")
		case COMMENT:
			continue // TODO: how to parse comments?
		case LAST_BLOCK:
			if err := w.Flush(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("chuck has unsupported compression type: %#x", chunk.Type)
		}
		bar.Increment()
	}
	// wait for our bar to complete and flush
	p.Wait()

	return nil
}

// DecompressChunk decompresses a given chunk and writes it to supplied bufio.Writer
func (chunk *udifBlockChunk) DecompressChunk(r *io.SectionReader, w *bufio.Writer) (n int, err error) {

	buff := make([]byte, chunk.CompressedLength)

	switch chunk.Type {
	case ZERO_FILL:
		n, err = w.Write(make([]byte, chunk.CompressedLength))
		if err != nil {
			return
		}
		log.Debugf("Wrote %#x bytes of ZERO_FILL data", n)
	case UNCOMPRESSED:
		if _, err = r.ReadAt(buff, int64(chunk.CompressedOffset)); err != nil {
			return
		}
		n, err = w.Write(buff)
		if err != nil {
			return
		}
		log.Debugf("Wrote %#x bytes of UNCOMPRESSED data", n)
	case IGNORED:
		n, err = w.Write(make([]byte, chunk.DiskLength*udifSectorSize))
		if err != nil {
			return
		}
		log.Debugf("Wrote %#x bytes of IGNORED data", n)
	case COMPRESS_ADC:
		if _, err = r.ReadAt(buff, int64(chunk.CompressedOffset)); err != nil {
			return
		}
		n, err = w.Write(adc.DecompressADC(buff))
		if err != nil {
			return
		}
		log.Debugf("Wrote %#x bytes of COMPRESS_ADC data", n)
	case COMPRESS_ZLIB:
		if _, err = r.ReadAt(buff, int64(chunk.CompressedOffset)); err != nil {
			return
		}
		var r io.ReadCloser
		r, err = zlib.NewReader(bytes.NewReader(buff))
		if err != nil {
			return
		}
		var nn int64
		nn, err = w.ReadFrom(r)
		if err != nil {
			return int(nn), err
		}
		r.Close()
		log.Debugf("Wrote %#x bytes of COMPRESS_ZLIB data", n)
	case COMPRESSS_BZ2:
		if _, err = r.ReadAt(buff, int64(chunk.CompressedOffset)); err != nil {
			return
		}
		var nn int64
		nn, err = w.ReadFrom(bzip2.NewReader(bytes.NewReader(buff)))
		if err != nil {
			return int(nn), err
		}
		log.Debugf("Wrote %#x bytes of COMPRESSS_BZ2 data", n)
	case COMPRESSS_LZFSE:
		if _, err = r.ReadAt(buff, int64(chunk.CompressedOffset)); err != nil {
			return
		}
		n, err = w.Write(lzfse.DecodeBuffer(buff))
		if err != nil {
			return
		}
		log.Debugf("Wrote %#x bytes of COMPRESSS_LZFSE data", n)
	case COMPRESSS_LZMA:
		return n, fmt.Errorf("COMPRESSS_LZMA is currently unsupported")
	case COMMENT: // TODO: how to parse comments?
	case LAST_BLOCK:
		if err = w.Flush(); err != nil {
			return
		}
	default:
		return n, fmt.Errorf("chuck has unsupported compression type: %#x", chunk.Type)
	}

	return
}

// Open opens the named file using os.Open and prepares it for use as a dmg.
func Open(name string) (*DMG, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	ff, err := NewDMG(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := ff.Load(); err != nil {
		return nil, err
	}
	ff.closer = f
	return ff, nil
}

// Close closes the DMG.
// If the DMG was created using NewFile directly instead of Open,
// Close has no effect.
func (d *DMG) Close() error {
	var err error
	if d.closer != nil {
		err = d.closer.Close()
		d.closer = nil
	}
	return err
}

// GetSize returns the size of the DMG data
func (d *DMG) GetSize() uint64 {
	return d.Footer.SectorCount * sectorSize
}

// GetBlock returns the size of the DMG data
func (d *DMG) GetBlock(name string) (*UDIFBlockData, error) {
	for _, block := range d.Blocks {
		if strings.EqualFold(block.Name, name) {
			return &block, nil
		}
	}
	return nil, fmt.Errorf("block %s not found", name)
}

// Load parses and verifies the GPT
func (d *DMG) Load() error {

	var out bytes.Buffer

	w := bufio.NewWriter(&out)

	block, err := d.GetBlock("Primary GPT Header")
	if err != nil {
		return fmt.Errorf("failed to load and verify GPT: %w", err)
	}

	for i, chunk := range block.Chunks {
		if _, err := chunk.DecompressChunk(d.sr, w); err != nil {
			return fmt.Errorf("failed to decompress chunk %d in block %s: %w", i, block.Name, err)
		}
	}

	var g gpt.GUIDPartitionTable
	if err := binary.Read(bytes.NewReader(out.Bytes()), binary.LittleEndian, &g.Header); err != nil {
		return fmt.Errorf("failed to read %T: %w", g.Header, err)
	}

	if err := g.Header.Verify(); err != nil {
		return fmt.Errorf("failed to verify GPT header: %w", err)
	}

	block, err = d.GetBlock("Primary GPT Table")
	if err != nil {
		return fmt.Errorf("failed to load and verify GPT: %w", err)
	}

	for i, chunk := range block.Chunks {
		if _, err := chunk.DecompressChunk(d.sr, w); err != nil {
			return fmt.Errorf("failed to decompress chunk %d in block %s: %w", i, block.Name, err)
		}
	}

	g.Partitions = make([]gpt.Partition, g.Header.EntriesCount)
	if err := binary.Read(bytes.NewReader(out.Bytes()), binary.LittleEndian, &g.Partitions); err != nil {
		return fmt.Errorf("failed to load and verify GPT: %w", err)
	}

	// Find first APFS partition
	found := false
	for _, part := range g.Partitions {
		if part.Type.String() == gpt.Apple_APFS {
			for i, block := range d.Blocks {
				if block.udifBlockData.StartSector == part.StartingLBA {
					found = true
					d.firstAPFSPartition = i
					// setup sector cache
					d.cache, err = lru.NewWithEvict(int(block.BuffersNeeded), func(k interface{}, v interface{}) {
						if k != v {
							log.Fatalf("cache failure: evict values not equal (%v!=%v)", k, v)
						}
						d.evictCounter++
					})
					if err != nil {
						return fmt.Errorf("failed to initialize DMG read cache: %w", err)
					}
				}
			}
			// Get partition offset and size
			d.apfsPartitionOffset = part.StartingLBA * sectorSize
			d.apfsPartitionSize = (part.EndingLBA - part.StartingLBA + 1) * sectorSize
		}
	}
	if !found {
		return fmt.Errorf("failed to find Apple_APFS partition in DMG")
	}

	return nil
}

// ReadAt impliments the io.ReadAt interface requirement of the Device interface
func (d *DMG) ReadAt(buf []byte, off int64) (n int, err error) {

	var (
		beg int
		mid int

		rdOffs int64
		rdSize int64

		cbuf bytes.Buffer
	)

	off += int64(d.apfsPartitionOffset) // map offset into start of Apple_APFS partition
	length := int64(len(buf))

	entryIdx := len(d.Blocks[d.firstAPFSPartition].Chunks)
	end := entryIdx - 1

	for beg <= end {
		mid = (beg + end) / 2
		if off >= int64(d.Blocks[d.firstAPFSPartition].Chunks[mid].DiskOffset) && off < int64(d.Blocks[d.firstAPFSPartition].Chunks[mid].DiskOffset+d.Blocks[d.firstAPFSPartition].Chunks[mid].DiskLength) {
			entryIdx = mid
			break
		} else if off < int64(d.Blocks[d.firstAPFSPartition].Chunks[mid].DiskOffset) {
			end = mid - 1
		} else {
			beg = mid + 1
		}
	}

	if int(entryIdx) == len(d.Blocks[d.firstAPFSPartition].Chunks)-1 {
		return n, fmt.Errorf("entryIdx == d.Blocks[d.firstAPFSPartition].Chunks ")
	}

	if val, found := d.cache.Get(entryIdx); found {
		cbuf = val.(bytes.Buffer)

		r := bytes.NewReader(cbuf.Bytes())

		r.Seek(off-int64(d.Blocks[d.firstAPFSPartition].Chunks[entryIdx].DiskOffset), io.SeekStart)

		if cbuf.Len()-int(off-int64(d.Blocks[d.firstAPFSPartition].Chunks[entryIdx].DiskOffset))-len(buf) > 0 {
			if err := binary.Read(r, binary.LittleEndian, &buf); err != nil {
				return n, fmt.Errorf("failed to fill buffer with cache buffer: %w", err)
			}

			return len(buf), nil
		}
	}

	w := bufio.NewWriter(&cbuf)

	for length > 0 {

		if int(entryIdx) >= len(d.Blocks[d.firstAPFSPartition].Chunks)-1 {
			return n, fmt.Errorf("entryIdx >= d.Blocks[d.firstAPFSPartition].Chunks ")
		}

		sect := d.Blocks[d.firstAPFSPartition].Chunks[entryIdx]

		rdOffs = off - int64(sect.DiskOffset)
		rdSize = length

		if rdOffs+rdSize > int64(sect.DiskLength) {
			rdSize = int64(sect.DiskLength) - rdOffs
		}

		rdSize, err := sect.DecompressChunk(d.sr, w)
		if err != nil {
			return n, fmt.Errorf("failed to decompress chunk at index %d: %w", entryIdx, err)
		}

		n += rdSize
		length -= int64(rdSize)
		entryIdx++
	}

	w.Flush()

	r := bytes.NewReader(cbuf.Bytes())

	r.Seek(off-int64(d.Blocks[d.firstAPFSPartition].Chunks[entryIdx-1].DiskOffset), io.SeekStart)

	if err := binary.Read(r, binary.LittleEndian, &buf); err != nil {
		return n, fmt.Errorf("failed to fill buffer with cache buffer: %w", err)
	}

	d.cache.Add(entryIdx-1, cbuf)

	return len(buf), nil
}

// NewDMG creates a new DMG for accessing a dmg in an underlying reader.
// The dmg is expected to start at position 0 in the ReaderAt.
func NewDMG(r *os.File) (*DMG, error) {

	d := new(DMG)
	d.sr = io.NewSectionReader(r, 0, 1<<63-1)

	if _, err := r.Seek(int64(-binary.Size(UDIFResourceFile{})), io.SeekEnd); err != nil {
		return nil, fmt.Errorf("failed to seek to DMG footer: %v", err)
	}

	if err := binary.Read(r, binary.BigEndian, &d.Footer); err != nil {
		return nil, fmt.Errorf("failed to read DMG footer: %v", err)
	}

	if d.Footer.Signature.String() != udifRFSignature {
		return nil, fmt.Errorf("found unexpected UDIFResourceFile signure: %s, expected: %s", d.Footer.Signature.String(), udifRFSignature)
	}

	// TODO: parse Code Signnature

	r.Seek(int64(d.Footer.PlistOffset), io.SeekStart)

	pdata := make([]byte, d.Footer.PlistLength)
	if err := binary.Read(r, binary.BigEndian, &pdata); err != nil {
		return nil, fmt.Errorf("failed to read DMG plist data: %v", err)
	}

	pl := plist.NewDecoder(bytes.NewReader(pdata))
	if err := pl.Decode(&d.Plist); err != nil {
		return nil, fmt.Errorf("failed to parse DMG plist data: %v\n%s", err, string(pdata[:]))
	}

	if nsiz, ok := d.Plist.ResourceFork["nsiz"]; ok {
		pl = plist.NewDecoder(bytes.NewReader(nsiz[0].Data))
		if err := pl.Decode(&d.Nsiz); err != nil {
			return nil, fmt.Errorf("failed to parse nsiz plist data: %v\n%s", err, string(nsiz[0].Data[:]))
		}
	}

	// TODO: handle 'cSum', 'plst' and 'size' also
	for _, block := range d.Plist.ResourceFork["blkx"] {
		var bdata UDIFBlockData

		r := bytes.NewReader(block.Data)

		bdata.Name = block.Name
		bdata.sr = d.sr

		if err := binary.Read(r, binary.BigEndian, &bdata.udifBlockData); err != nil {
			return nil, fmt.Errorf("failed to read UDIFBlockData in block %s: %v", block.Name, err)
		}

		if bdata.udifBlockData.Signature.String() != udifBDSignature {
			return nil, fmt.Errorf("found unexpected UDIFBlockData signure: %s, expected: %s", bdata.udifBlockData.Signature.String(), udifBDSignature)
		}

		for i := 0; i < int(bdata.udifBlockData.ChunkCount); i++ {
			var chunk udifBlockChunk
			binary.Read(r, binary.BigEndian, &chunk)
			bdata.Chunks = append(bdata.Chunks, udifBlockChunk{
				Type:             chunk.Type,
				Comment:          chunk.Comment,
				DiskOffset:       (chunk.DiskOffset + bdata.StartSector) * sectorSize,
				DiskLength:       chunk.DiskLength * sectorSize,
				CompressedOffset: chunk.CompressedOffset + bdata.DataOffset,
				CompressedLength: chunk.CompressedLength,
			})
		}

		d.Blocks = append(d.Blocks, bdata)
	}

	return d, nil
}
