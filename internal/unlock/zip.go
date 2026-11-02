package unlock

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const maxCentralDirectoryBytes = 32 << 20
const maxDirectoryTailBytes = maxCentralDirectoryBytes + (128 << 10) + 128

type directoryBounds struct{ start, end int64 }

type directorySnapshot struct {
	start int64
	data  []byte
}

func (s *directorySnapshot) ReadAt(p []byte, offset int64) (int, error) {
	if offset < s.start {
		return 0, errors.New("ZIP directory moved outside its validated snapshot")
	}
	return bytes.NewReader(s.data).ReadAt(p, offset-s.start)
}

type archiveSnapshotReader struct {
	reader    io.ReaderAt
	directory *directorySnapshot
}

func (r archiveSnapshotReader) ReadAt(p []byte, offset int64) (int, error) {
	if offset >= r.directory.start {
		return r.directory.ReadAt(p, offset)
	}
	prefix := r.directory.start - offset
	if int64(len(p)) <= prefix {
		return r.reader.ReadAt(p, offset)
	}
	n, err := r.reader.ReadAt(p[:prefix], offset)
	if err != nil {
		return n, err
	}
	m, err := r.directory.ReadAt(p[prefix:], r.directory.start)
	return n + m, err
}

func prepareZIPReader(ctx context.Context, source io.ReaderAt, size int64) (*guardedReaderAt, error) {
	reader := &guardedReaderAt{ctx: ctx, reader: source, remaining: maxDirectoryTailBytes}
	var bounds directoryBounds
	if err := preflightDirectory(ctx, reader, size, &bounds); err != nil {
		return nil, err
	}
	start := bounds.start
	tailStart := size - 65*1024
	if tailStart < 0 {
		tailStart = 0
	}
	if tailStart < start {
		start = tailStart
	}
	snapshot := &directorySnapshot{start: start, data: make([]byte, int(size-start))}
	reader.remaining = maxDirectoryTailBytes
	if _, err := reader.ReadAt(snapshot.data, snapshot.start); err != nil {
		return nil, err
	}
	// Input may be concurrently writable. Recheck the immutable directory,
	// not the live file, so a change between the preflight and zip.NewReader
	// cannot reintroduce unchecked counts or allocation-heavy records.
	reader.reader = snapshot
	reader.remaining = maxDirectoryTailBytes
	var validated directoryBounds
	if err := preflightDirectory(ctx, reader, size, &validated); err != nil {
		return nil, err
	}
	// Go's ZIP reader searches backwards for the first apparent end signature
	// even inside a legitimate archive comment. Supply a comment-free view
	// after validating the real end record, and restore the original comment
	// on zip.Reader for verbatim preservation when writing the output.
	end := validated.end - snapshot.start
	reader.archiveComment = string(snapshot.data[end+22:])
	reader.archiveSize = validated.end + 22
	snapshot.data[end+20], snapshot.data[end+21] = 0, 0
	reader.reader = archiveSnapshotReader{reader: source, directory: snapshot}
	reader.remaining = maxDirectoryTailBytes
	return reader, nil
}

type guardedReaderAt struct {
	ctx            context.Context
	reader         io.ReaderAt
	remaining      int64 // Negative after directory parsing; entry reads are separately bounded.
	archiveSize    int64
	archiveComment string
}

func (r *guardedReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.remaining >= 0 {
		if int64(len(p)) > r.remaining {
			return 0, errors.New("ZIP directory parsing exceeded its read budget")
		}
		r.remaining -= int64(len(p))
	}
	return r.reader.ReadAt(p, offset)
}

// preflightDirectory bounds allocations BEFORE archive/zip creates File objects.
// Counts in EOCD are untrusted: walk the actual central records with a fixed
// header buffer, without allocating their names, extra fields, or comments.
func preflightDirectory(ctx context.Context, reader io.ReaderAt, size int64, bounds ...*directoryBounds) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if size < 22 {
		return errors.New("invalid ZIP: missing end of central directory")
	}
	tailSize := int64(65 * 1024)
	if tailSize > size {
		tailSize = size
	}
	tail := make([]byte, int(tailSize))
	if _, err := reader.ReadAt(tail, size-tailSize); err != nil {
		return err
	}
	endIndex := -1
	sawEnd := false
	// End-signature-looking bytes are legal inside comments. Only consider
	// candidates whose declared comment ends exactly at the archive boundary.
	for i := len(tail) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) == 0x06054b50 {
			sawEnd = true
			comment := int(binary.LittleEndian.Uint16(tail[i+20:]))
			if i+22+comment != len(tail) {
				continue
			}
			endIndex = i
			break
		}
	}
	if endIndex < 0 {
		if sawEnd {
			return errors.New("invalid ZIP end record or trailing data")
		}
		return errors.New("invalid ZIP: missing end of central directory")
	}
	end := tail[endIndex:]
	endOffset := size - tailSize + int64(endIndex)
	physicalEnd := endOffset
	if binary.LittleEndian.Uint16(end[4:]) != 0 || binary.LittleEndian.Uint16(end[6:]) != 0 {
		return errors.New("multi-disk ZIP archives are not supported")
	}
	countDisk := uint64(binary.LittleEndian.Uint16(end[8:]))
	count := uint64(binary.LittleEndian.Uint16(end[10:]))
	directorySize := uint64(binary.LittleEndian.Uint32(end[12:]))
	directoryOffset := uint64(binary.LittleEndian.Uint32(end[16:]))
	trigger64 := count == 0xffff || directorySize == 0xffff || directorySize == 0xffffffff || directoryOffset == 0xffffffff
	if trigger64 {
		var locator [20]byte
		hasLocator := false
		if endOffset >= 20 {
			if _, err := reader.ReadAt(locator[:], endOffset-20); err != nil {
				return err
			}
			hasLocator = binary.LittleEndian.Uint32(locator[:]) == 0x07064b50
		}
		if hasLocator {
			if binary.LittleEndian.Uint32(locator[4:]) != 0 || binary.LittleEndian.Uint32(locator[16:]) != 1 {
				return errors.New("multi-disk ZIP64 archives are not supported")
			}
			offset64 := binary.LittleEndian.Uint64(locator[8:])
			if offset64 > uint64(endOffset-20) || uint64(endOffset-20)-offset64 < 56 {
				return errors.New("invalid ZIP64 directory location")
			}
			var header [56]byte
			if _, err := reader.ReadAt(header[:], int64(offset64)); err != nil {
				return err
			}
			length := binary.LittleEndian.Uint64(header[4:])
			if binary.LittleEndian.Uint32(header[:]) != 0x06064b50 || length < 44 ||
				length != uint64(endOffset-20)-offset64-12 {
				return errors.New("invalid ZIP64 directory record")
			}
			if binary.LittleEndian.Uint32(header[16:]) != 0 || binary.LittleEndian.Uint32(header[20:]) != 0 {
				return errors.New("multi-disk ZIP64 archives are not supported")
			}
			countDisk = binary.LittleEndian.Uint64(header[24:])
			count = binary.LittleEndian.Uint64(header[32:])
			directorySize = binary.LittleEndian.Uint64(header[40:])
			directoryOffset = binary.LittleEndian.Uint64(header[48:])
			endOffset = int64(offset64)
		} else if count == 0xffff || directorySize == 0xffffffff || directoryOffset == 0xffffffff {
			return errors.New("invalid ZIP64: missing directory locator")
		}
	}
	if count > maxEntries {
		return errors.New("archive exceeds the 10000-entry limit")
	}
	if directorySize > maxCentralDirectoryBytes {
		return errors.New("ZIP central directory exceeds the 32 MiB limit")
	}
	// No self-extracting prefix, overlapping directory, or parser base-offset
	// correction: archive/zip must read exactly the directory checked here.
	if directoryOffset > uint64(endOffset) || directorySize != uint64(endOffset)-directoryOffset {
		return errors.New("invalid ZIP central directory bounds")
	}
	if uint64(size)-directoryOffset > maxDirectoryTailBytes {
		return errors.New("ZIP directory metadata exceeds its bounded size limit")
	}
	cursor := int64(directoryOffset)
	actual := uint64(0)
	var header [46]byte
	for cursor < endOffset {
		if err := ctx.Err(); err != nil {
			return err
		}
		if endOffset-cursor < int64(len(header)) {
			return errors.New("truncated ZIP central directory record")
		}
		if _, err := reader.ReadAt(header[:], cursor); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(header[:]) != 0x02014b50 {
			return errors.New("invalid ZIP central directory record")
		}
		actual++
		if actual > maxEntries {
			return errors.New("archive exceeds the 10000-entry limit")
		}
		length := int64(len(header)) + int64(binary.LittleEndian.Uint16(header[28:])) +
			int64(binary.LittleEndian.Uint16(header[30:])) + int64(binary.LittleEndian.Uint16(header[32:]))
		if length > endOffset-cursor {
			return errors.New("truncated ZIP central directory fields")
		}
		cursor += length
	}
	if actual != count || countDisk != count {
		return fmt.Errorf("invalid ZIP entry count: declared %d, actual %d", count, actual)
	}
	if len(bounds) != 0 {
		bounds[0].start = int64(directoryOffset)
		bounds[0].end = physicalEnd
	}
	return nil
}
