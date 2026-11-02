package unlock

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

type readerAtFunc func([]byte, int64) (int, error)

func (read readerAtFunc) ReadAt(p []byte, offset int64) (int, error) {
	return read(p, offset)
}

type measuredReaderAt struct {
	reader io.ReaderAt
	bytes  int64
	reads  int
	cancel context.CancelFunc
}

func (r *measuredReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	r.bytes += int64(len(p))
	r.reads++
	if r.cancel != nil && r.reads == 4 {
		r.cancel()
	}
	return r.reader.ReadAt(p, offset)
}

// All central records reference a single tiny local entry. The deliberately
// forged EOCD count must never permit unbounded archive/zip allocations.
func manyDirectoryRecords(actual, declared int) []byte {
	data := make([]byte, 31, 31+47*actual+22)
	binary.LittleEndian.PutUint32(data, 0x04034b50)
	binary.LittleEndian.PutUint16(data[4:], 20)
	binary.LittleEndian.PutUint16(data[26:], 1)
	data[30] = 'a'
	for i := 0; i < actual; i++ {
		var header [47]byte
		binary.LittleEndian.PutUint32(header[:], 0x02014b50)
		binary.LittleEndian.PutUint16(header[4:], 20)
		binary.LittleEndian.PutUint16(header[6:], 20)
		binary.LittleEndian.PutUint16(header[28:], 1)
		header[46] = 'a'
		data = append(data, header[:]...)
	}
	var end [22]byte
	binary.LittleEndian.PutUint32(end[:], 0x06054b50)
	binary.LittleEndian.PutUint16(end[8:], uint16(declared))
	binary.LittleEndian.PutUint16(end[10:], uint16(declared))
	binary.LittleEndian.PutUint32(end[12:], uint32(47*actual))
	binary.LittleEndian.PutUint32(end[16:], 31)
	return append(data, end[:]...)
}

func TestDirectoryPreflightCountsActualRecords(t *testing.T) {
	dir := testDirectory(t)
	for _, counts := range [][2]int{{maxEntries + 100, 0}, {maxEntries + 100, 1}, {maxEntries + 100, maxEntries}, {65536, 0}} {
		actual, declared := counts[0], counts[1]
		data := manyDirectoryRecords(actual, declared)
		reader := &measuredReaderAt{reader: bytes.NewReader(data)}
		err := preflightDirectory(context.Background(), reader, int64(len(data)))
		if err == nil || !strings.Contains(err.Error(), "10000-entry") {
			t.Fatalf("declared %d: error = %v", declared, err)
		}
		if reader.reads > maxEntries+2 || reader.bytes > 65*1024+46*(maxEntries+1) {
			t.Fatalf("unbounded directory reads: %d reads, %d bytes", reader.reads, reader.bytes)
		}
		input := filepath.Join(dir, "forged.xlsx")
		writeBytes(t, input, data)
		if _, err := Inspect(context.Background(), input); err == nil || !strings.Contains(err.Error(), "10000-entry") {
			t.Fatalf("Inspect: %v", err)
		}
	}
}

func TestDirectoryPreflightCancellationAndReadBudget(t *testing.T) {
	data := manyDirectoryRecords(maxEntries, maxEntries)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &measuredReaderAt{reader: bytes.NewReader(data), cancel: cancel}
	err := preflightDirectory(ctx, reader, int64(len(data)))
	if !errors.Is(err, context.Canceled) || reader.reads > 4 {
		t.Fatalf("cancellation = %v after %d reads", err, reader.reads)
	}
	guarded := &guardedReaderAt{ctx: context.Background(), reader: bytes.NewReader(data), remaining: 8}
	if _, err := guarded.ReadAt(make([]byte, 9), 0); err == nil || !strings.Contains(err.Error(), "read budget") {
		t.Fatalf("budget error = %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	guarded = &guardedReaderAt{ctx: ctx, reader: bytes.NewReader(data), remaining: -1}
	if _, err := guarded.ReadAt(make([]byte, 1), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReaderAt cancellation = %v", err)
	}
}

func TestDirectorySnapshotIsImmutableBeforeZIPAllocation(t *testing.T) {
	data := archiveBytes(t, fixture(true, 1, false), "")
	reader, err := prepareZIPReader(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	endOffset := bytes.LastIndex(data, []byte{'P', 'K', 5, 6})
	// Mutate the live EOCD after preflight. archive/zip must still see the
	// independently validated immutable copy rather than allocate from this.
	binary.LittleEndian.PutUint16(data[endOffset+10:], 0xffff)
	zr, err := zip.NewReader(reader, reader.archiveSize)
	if err != nil || len(zr.File) != len(fixture(true, 1, false)) {
		t.Fatalf("immutable directory parse = %v", err)
	}
}

func TestEmbeddedEndSignatureInArchiveComment(t *testing.T) {
	dir := testDirectory(t)
	for _, fakeCommentLength := range []uint16{0, 0xffff} {
		t.Run(fmt.Sprint(fakeCommentLength), func(t *testing.T) {
			comment := bytes.Repeat([]byte("x"), 80)
			copy(comment[11:], []byte{'P', 'K', 5, 6})
			binary.LittleEndian.PutUint16(comment[31:], fakeCommentLength)
			data := archiveBytes(t, fixture(true, 1, false), "")
			end := bytes.LastIndex(data, []byte{'P', 'K', 5, 6})
			data = append([]byte(nil), data[:end+22]...)
			binary.LittleEndian.PutUint16(data[end+20:], uint16(len(comment)))
			data = append(data, comment...)
			if err := preflightDirectory(context.Background(), bytes.NewReader(data), int64(len(data))); err != nil {
				t.Fatalf("valid embedded end signature rejected: %v", err)
			}
			input := filepath.Join(dir, fmt.Sprintf("in%d.xlsx", fakeCommentLength))
			output := filepath.Join(dir, fmt.Sprintf("out%d.xlsx", fakeCommentLength))
			writeBytes(t, input, data)
			if info, err := Inspect(context.Background(), input); err != nil || info != (Info{Sheets: 1, Workbook: true}) {
				t.Fatalf("Inspect = %+v, %v", info, err)
			}
			if _, err := Unlock(context.Background(), input, output, false, nil); err != nil {
				t.Fatalf("Unlock valid comment: %v", err)
			}
			if info, err := Inspect(context.Background(), output); err != nil || info.Protected() {
				t.Fatalf("output = %+v, %v", info, err)
			}
			result := readBytes(t, output)
			if !bytes.HasSuffix(result, comment) {
				t.Fatal("archive comment was not preserved verbatim")
			}
			outputEnd := len(result) - len(comment) - 22
			if binary.LittleEndian.Uint32(result[outputEnd:]) != 0x06054b50 ||
				int(binary.LittleEndian.Uint16(result[outputEnd+20:])) != len(comment) {
				t.Fatal("output archive end or comment length changed")
			}
			if !bytes.Equal(readBytes(t, input), data) {
				t.Fatal("input archive changed")
			}
		})
	}
}

func TestDirectorySnapshotRevalidatesConcurrentChanges(t *testing.T) {
	data := archiveBytes(t, fixture(true, 1, false), "")
	endOffset := bytes.LastIndex(data, []byte{'P', 'K', 5, 6})
	fullReads := 0
	source := readerAtFunc(func(p []byte, offset int64) (int, error) {
		if offset == 0 && len(p) == len(data) {
			fullReads++
			if fullReads == 2 {
				// This is the immutable copy read, after the first preflight.
				binary.LittleEndian.PutUint16(data[endOffset+10:], 0xffff)
			}
		}
		return bytes.NewReader(data).ReadAt(p, offset)
	})
	if _, err := prepareZIPReader(context.Background(), source, int64(len(data))); err == nil || !strings.Contains(err.Error(), "ZIP64") {
		t.Fatalf("snapshot mutation error = %v", err)
	}
}

func TestDirectoryPreflightRejectsMalformedBounds(t *testing.T) {
	valid := manyDirectoryRecords(1, 1)
	for _, tc := range []struct {
		name string
		edit func([]byte) []byte
		want string
	}{
		{"wrong-count", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[len(b)-12:], 0)
			return b
		}, "entry count"},
		{"wrong-disk-count", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[len(b)-14:], 0)
			return b
		}, "entry count"},
		{"multi-disk", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[len(b)-18:], 1)
			return b
		}, "multi-disk"},
		{"oversize-directory", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[len(b)-10:], maxCentralDirectoryBytes+1)
			return b
		}, "32 MiB"},
		{"wrong-offset", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[len(b)-6:], 32)
			return b
		}, "bounds"},
		{"truncated-fields", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[31+28:], 100)
			return b
		}, "truncated"},
		{"invalid-central-signature", func(b []byte) []byte {
			b[31] = 0
			return b
		}, "record"},
		{"trailing-data", func(b []byte) []byte { return append(b, 1) }, "trailing"},
		{"truncated-end", func(b []byte) []byte { return b[:len(b)-1] }, "end"},
		{"zip64-no-locator", func(b []byte) []byte {
			binary.LittleEndian.PutUint16(b[len(b)-12:], 0xffff)
			return b
		}, "ZIP64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.edit(append([]byte(nil), valid...))
			err := preflightDirectory(context.Background(), bytes.NewReader(data), int64(len(data)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v; want %q", err, tc.want)
			}
		})
	}
}

func withZIP64End(t *testing.T, data []byte) []byte {
	t.Helper()
	endOffset := bytes.LastIndex(data, []byte{'P', 'K', 5, 6})
	if endOffset < 0 {
		t.Fatal("no ZIP end record")
	}
	end := append([]byte(nil), data[endOffset:]...)
	var extended [56]byte
	binary.LittleEndian.PutUint32(extended[:], 0x06064b50)
	binary.LittleEndian.PutUint64(extended[4:], 44)
	binary.LittleEndian.PutUint16(extended[12:], 45)
	binary.LittleEndian.PutUint16(extended[14:], 45)
	count := uint64(binary.LittleEndian.Uint16(end[10:]))
	binary.LittleEndian.PutUint64(extended[24:], count)
	binary.LittleEndian.PutUint64(extended[32:], count)
	binary.LittleEndian.PutUint64(extended[40:], uint64(binary.LittleEndian.Uint32(end[12:])))
	binary.LittleEndian.PutUint64(extended[48:], uint64(binary.LittleEndian.Uint32(end[16:])))
	var locator [20]byte
	binary.LittleEndian.PutUint32(locator[:], 0x07064b50)
	binary.LittleEndian.PutUint64(locator[8:], uint64(endOffset))
	binary.LittleEndian.PutUint32(locator[16:], 1)
	binary.LittleEndian.PutUint16(end[8:], 0xffff)
	binary.LittleEndian.PutUint16(end[10:], 0xffff)
	binary.LittleEndian.PutUint32(end[12:], 0xffffffff)
	binary.LittleEndian.PutUint32(end[16:], 0xffffffff)
	result := append([]byte(nil), data[:endOffset]...)
	result = append(result, extended[:]...)
	result = append(result, locator[:]...)
	return append(result, end...)
}

func TestZIP64PreflightAndWorkbook(t *testing.T) {
	dir := testDirectory(t)
	data := withZIP64End(t, archiveBytes(t, fixture(true, 1, true), ""))
	if err := preflightDirectory(context.Background(), bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	input, output := filepath.Join(dir, "in.xlsm"), filepath.Join(dir, "out.xlsm")
	writeBytes(t, input, data)
	if info, err := Unlock(context.Background(), input, output, false, nil); err != nil || info != (Info{Sheets: 1, Workbook: true, MacroEnabled: true}) {
		t.Fatalf("ZIP64 Unlock = %+v, %v", info, err)
	}
	for _, tc := range []struct {
		name string
		edit func([]byte)
		want string
	}{
		{"forged-count", func(b []byte) {
			offset := bytes.Index(b, []byte{'P', 'K', 6, 6})
			binary.LittleEndian.PutUint64(b[offset+32:], 1)
		}, "entry count"},
		{"huge-count", func(b []byte) {
			offset := bytes.Index(b, []byte{'P', 'K', 6, 6})
			binary.LittleEndian.PutUint64(b[offset+32:], 1<<63)
		}, "10000-entry"},
		{"huge-offset", func(b []byte) {
			offset := bytes.Index(b, []byte{'P', 'K', 6, 7})
			binary.LittleEndian.PutUint64(b[offset+8:], 1<<63)
		}, "location"},
		{"invalid-length", func(b []byte) {
			offset := bytes.Index(b, []byte{'P', 'K', 6, 6})
			binary.LittleEndian.PutUint64(b[offset+4:], 43)
		}, "record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := append([]byte(nil), data...)
			tc.edit(bad)
			err := preflightDirectory(context.Background(), bytes.NewReader(bad), int64(len(bad)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v; want %q", err, tc.want)
			}
		})
	}
}
