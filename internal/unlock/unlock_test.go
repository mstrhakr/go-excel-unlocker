package unlock

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"
)

func testDirectory(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func writeBytes(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func readBytes(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func archiveBytes(t *testing.T, parts map[string][]byte, duplicate string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	writer.SetComment("preserve archive entries")
	var names []string
	for name := range parts {
		names = append(names, name)
	}
	sort.Strings(names)
	if duplicate != "" {
		names = append(names, duplicate)
	}
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate, Comment: "entry comment", Extra: []byte{0xfe, 0xca, 0, 0}}
		header.SetMode(0600)
		out, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = out.Write(parts[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The worksheet deliberately has neither an xl/ prefix nor an XML extension.
// The decoy has a conventional worksheet path but is not a workbook sheet.
func fixture(book bool, sheets int, macro bool) map[string][]byte {
	kind := workbookType
	if macro {
		kind = macroBookType
	}
	parts := map[string][]byte{
		"[Content_Types].xml": []byte(fmt.Sprintf(`<Types xmlns="%s"><Default Extension="xml" ContentType="application/xml"/><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="bin" ContentType="application/octet-stream"/><Override PartName="/book/main.xml" ContentType="%s"/><Override PartName="/sheets/real.dat" ContentType="%s"/></Types>`, contentNS, kind, worksheetType)),
		"_rels/.rels":         []byte(fmt.Sprintf(`<Relationships xmlns="%s"><Relationship Id="main" Type="%s/officeDocument" Target="book/main.xml"/></Relationships>`, packageRelNS, officeRelNS)),
		"book/main.xml": []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<workbook xmlns="%s" xmlns:r="%s"><!--keep workbook comment-->%s<sheets><sheet name="One" sheetId="1" r:id="sheet1"/></sheets></workbook>`, spreadsheetNS, officeRelNS, protectionString(book, "workbookProtection"))),
		"book/_rels/main.xml.rels": []byte(fmt.Sprintf(`<Relationships xmlns="%s"><Relationship Id="sheet1" Type="%s/worksheet" Target="../sheets/real.dat"/><Relationship Id="web" Type="%s/hyperlink" Target="https://example.invalid/" TargetMode="External"/></Relationships>`, packageRelNS, officeRelNS, officeRelNS)),
		"sheets/real.dat":          []byte(fmt.Sprintf("<?xml version=\"1.0\"?>\r\n<worksheet xmlns=\"%s\" xmlns:custom=\"urn:custom\">\r\n<sheetData><row r=\"1\"><c r=\"A1\" t=\"inlineStr\"><is><t>Keep &amp; preserve &lt;sheetProtection/&gt;</t></is></c></row></sheetData><!-- keep > comment -->%s<extLst><custom:sheetProtection custom:value=\"preserve\"/></extLst>\r\n</worksheet>", spreadsheetNS, strings.Repeat(`<sheetProtection password = 'ABCD' sheet="1"></sheetProtection>`, sheets))),
		"xl/worksheets/decoy.xml":  []byte(`<custom xmlns="urn:custom"><sheetProtection xmlns="urn:custom"/></custom>`),
		"assets/opaque.bin":        {0, 1, 0xff, 0xfe, 0, 9},
	}
	if macro {
		parts["vba/project.bin"] = []byte{0xd0, 0xcf, 0x11, 0xe0, 0xff, 0, 7}
		parts["[Content_Types].xml"] = bytes.Replace(parts["[Content_Types].xml"], []byte("</Types>"), []byte(`<Override PartName="/vba/project.bin" ContentType="application/vnd.ms-office.vbaProject"/></Types>`), 1)
		parts["book/_rels/main.xml.rels"] = bytes.Replace(parts["book/_rels/main.xml.rels"], []byte("</Relationships>"), []byte(fmt.Sprintf(`<Relationship Id="vba" Type="%s/vbaProject" Target="../vba/project.bin"/></Relationships>`, officeRelNS)), 1)
	}
	return parts
}

func protectionString(enabled bool, local string) string {
	if !enabled {
		return ""
	}
	return "<" + local + ` lockStructure="1" workbookPassword="FADE" />`
}

func TestProtectionCombinationsAndPreservation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		book   bool
		sheets int
		macro  bool
	}{
		{"none", false, 0, false},
		{"workbook", true, 0, false},
		{"worksheet", false, 1, false},
		{"multiple-elements", false, 3, false},
		{"combined", true, 2, false},
		{"macro", true, 1, true},
		{"unprotected-macro", false, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := testDirectory(t)
			ext := ".xlsx"
			if tc.macro {
				ext = ".xlsm"
			}
			input, output := filepath.Join(dir, "source"+ext), filepath.Join(dir, "result"+ext)
			parts := fixture(tc.book, tc.sheets, tc.macro)
			original := archiveBytes(t, parts, "")
			writeBytes(t, input, original)
			want := Info{Sheets: tc.sheets, Workbook: tc.book, MacroEnabled: tc.macro}
			info, err := Inspect(context.Background(), input)
			if err != nil || info != want {
				t.Fatalf("Inspect = %+v, %v; want %+v", info, err, want)
			}
			var stages []string
			got, err := Unlock(context.Background(), input, output, false, func(s string) { stages = append(stages, s) })
			if got != want {
				t.Fatalf("Unlock info = %+v; want %+v", got, want)
			}
			if !want.Protected() {
				if err == nil || !strings.Contains(err.Error(), "no worksheet or workbook protection") {
					t.Fatalf("unprotected error = %v", err)
				}
				if _, err = os.Stat(output); !os.IsNotExist(err) {
					t.Fatal("unprotected workbook created output")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if len(stages) != 4 {
					t.Fatalf("progress = %v", stages)
				}
				clean, err := Inspect(context.Background(), output)
				if err != nil || clean.Protected() || clean.MacroEnabled != tc.macro {
					t.Fatalf("output inspection = %+v, %v", clean, err)
				}
				assertPreserved(t, input, output, parts)
			}
			if !bytes.Equal(readBytes(t, input), original) {
				t.Fatal("original changed")
			}
			assertNoTemps(t, dir)
		})
	}
}

func assertPreserved(t *testing.T, input, output string, parts map[string][]byte, additionalChanges ...map[string][]byte) {
	t.Helper()
	in, err := zip.OpenReader(input)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := zip.OpenReader(output)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if len(in.File) != len(out.File) {
		t.Fatal("entry count changed")
	}
	if in.Comment != out.Comment {
		t.Fatal("archive comment changed")
	}
	for i, entry := range out.File {
		before := in.File[i]
		if entry.Name != before.Name || entry.Comment != before.Comment || entry.Method != before.Method ||
			entry.ExternalAttrs != before.ExternalAttrs || !bytes.Equal(entry.Extra, before.Extra) {
			t.Fatalf("entry metadata or order changed: %s", entry.Name)
		}
		expected := parts[entry.Name]
		if entry.Name == "book/main.xml" {
			expected = bytes.ReplaceAll(expected, []byte(protectionString(true, "workbookProtection")), nil)
		}
		if entry.Name == "sheets/real.dat" {
			expected = bytes.ReplaceAll(expected, []byte(`<sheetProtection password = 'ABCD' sheet="1"></sheetProtection>`), nil)
		}
		for _, changes := range additionalChanges {
			if changed, ok := changes[entry.Name]; ok {
				expected = changed
			}
		}
		r, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		actual, err := io.ReadAll(r)
		r.Close()
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("unexpected bytes for %s: %v", entry.Name, err)
		}
		if bytes.Equal(expected, parts[entry.Name]) {
			a, _ := before.OpenRaw()
			b, _ := entry.OpenRaw()
			rawA, _ := io.ReadAll(a)
			rawB, _ := io.ReadAll(b)
			if !bytes.Equal(rawA, rawB) {
				t.Fatalf("unrelated compressed bytes changed: %s", entry.Name)
			}
		}
	}
}

func TestMultipleDistinctProtectedWorksheets(t *testing.T) {
	dir := testDirectory(t)
	parts := fixture(true, 1, true)
	parts["[Content_Types].xml"] = bytes.Replace(parts["[Content_Types].xml"], []byte("</Types>"), []byte(
		`<Override PartName="/different/second.xml" ContentType="`+worksheetType+`"/>`+
			`<Override PartName="/different/plain.xml" ContentType="`+worksheetType+`"/></Types>`), 1)
	parts["book/main.xml"] = bytes.Replace(parts["book/main.xml"], []byte("</sheets>"), []byte(
		`<sheet name="Second" sheetId="2" r:id="sheet2"/><sheet name="Plain" sheetId="3" r:id="sheet3"/></sheets>`), 1)
	parts["book/_rels/main.xml.rels"] = bytes.Replace(parts["book/_rels/main.xml.rels"], []byte("</Relationships>"), []byte(
		`<Relationship Id="sheet2" Type="`+officeRelNS+`/worksheet" Target="../different/second.xml"/>`+
			`<Relationship Id="sheet3" Type="`+officeRelNS+`/worksheet" Target="../different/plain.xml"/></Relationships>`), 1)
	secondProtection := `<s:sheetProtection sheet="1" objects="1" scenarios="1" />`
	second := `<?xml version="1.0" encoding="UTF-8"?>` + "\r\n" +
		`<s:worksheet xmlns:s="` + spreadsheetNS + `" xmlns:foreign="urn:extension"><s:sheetData/><!--second sheet comment-->` +
		secondProtection + `<s:extLst><foreign:sheetProtection keep="yes"/></s:extLst></s:worksheet>`
	parts["different/second.xml"] = []byte(second)
	parts["different/plain.xml"] = []byte(`<worksheet xmlns="` + spreadsheetNS + `" xmlns:foreign="urn:extension"><sheetData/><foreign:sheetProtection keep="yes"/></worksheet>`)
	parts["different/unrelated.xml"] = []byte(`<?xml version="1.0"?><metadata xmlns="urn:custom">Do not change any bytes.</metadata>`)
	original := archiveBytes(t, parts, "")
	input, output := filepath.Join(dir, "three-sheets.xlsm"), filepath.Join(dir, "unlocked.xlsm")
	writeBytes(t, input, original)
	want := Info{Sheets: 2, Workbook: true, MacroEnabled: true}
	if info, err := Inspect(context.Background(), input); err != nil || info != want {
		t.Fatalf("Inspect three actual sheet parts = %+v, %v; want %+v", info, err, want)
	}
	if info, err := Unlock(context.Background(), input, output, false, nil); err != nil || info != want {
		t.Fatalf("Unlock three actual sheet parts = %+v, %v; want %+v", info, err, want)
	}
	if info, err := Inspect(context.Background(), output); err != nil || info.Protected() || !info.MacroEnabled {
		t.Fatalf("result = %+v, %v", info, err)
	}
	assertPreserved(t, input, output, parts, map[string][]byte{
		"different/second.xml": []byte(strings.Replace(second, secondProtection, "", 1)),
	})
	if !bytes.Equal(readBytes(t, input), original) {
		t.Fatal("original multi-sheet workbook changed")
	}
	assertNoTemps(t, dir)
}

func assertNoTemps(t *testing.T, dir string) {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, ".excel-unlock-*"))
	if err != nil || len(names) != 0 {
		t.Fatalf("temporary outputs remain: %v, %v", names, err)
	}
}

func TestSurgicalXML(t *testing.T) {
	for _, tc := range []struct {
		name, xml, want string
		count           int
	}{
		{"default", `<worksheet xmlns="` + spreadsheetNS + `"><sheetProtection/></worksheet>`, `<worksheet xmlns="` + spreadsheetNS + `"></worksheet>`, 1},
		{"prefixed", `<s:worksheet xmlns:s="` + spreadsheetNS + `"><s:sheetProtection  password = "a&gt;b" /></s:worksheet>`, `<s:worksheet xmlns:s="` + spreadsheetNS + `"></s:worksheet>`, 1},
		{"strict", `<worksheet xmlns="` + strictSheetNS + `"><sheetProtection/><sheetProtection></sheetProtection></worksheet>`, `<worksheet xmlns="` + strictSheetNS + `"></worksheet>`, 2},
		{"nested-content", `<worksheet xmlns="` + spreadsheetNS + `"><sheetProtection><ignored><![CDATA[a>b]]></ignored></sheetProtection><sheetData/></worksheet>`, `<worksheet xmlns="` + spreadsheetNS + `"><sheetData/></worksheet>`, 1},
		{"foreign", `<worksheet xmlns="` + spreadsheetNS + `"><sheetProtection xmlns="urn:other"/><extLst><sheetProtection/></extLst></worksheet>`, `<worksheet xmlns="` + spreadsheetNS + `"><sheetProtection xmlns="urn:other"/><extLst><sheetProtection/></extLst></worksheet>`, 0},
		{"lexical-preservation", `<?xml version="1.0"?><!-- pre -->` + "\r\n" + `<worksheet xmlns="` + spreadsheetNS + `"><![CDATA[<sheetProtection/>]]><sheetProtection/> <!--post--></worksheet>`, `<?xml version="1.0"?><!-- pre -->` + "\r\n" + `<worksheet xmlns="` + spreadsheetNS + `"><![CDATA[<sheetProtection/>]]> <!--post--></worksheet>`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, n, err := protectionXML(context.Background(), []byte(tc.xml), "worksheet")
			if err != nil || string(got) != tc.want || n != tc.count {
				t.Fatalf("got %q, %d, %v; want %q, %d", got, n, err, tc.want, tc.count)
			}
		})
	}
}

func TestRejectInvalidXML(t *testing.T) {
	for _, document := range []string{
		``,
		`<worksheet>`,
		`<worksheet></workbook>`,
		`<worksheet xmlns="` + spreadsheetNS + `"/>trailing`,
		`junk<worksheet xmlns="` + spreadsheetNS + `"/>`,
		`<worksheet xmlns="` + spreadsheetNS + `"/><another/>`,
		`<!DOCTYPE worksheet><worksheet xmlns="` + spreadsheetNS + `"/>`,
		`<worksheet xmlns="` + spreadsheetNS + `" a="1" a="2"/>`,
		`<worksheet xmlns="` + spreadsheetNS + `" xmlns:a="urn:a" xmlns:b="urn:a" a:x="1" b:x="2"/>`,
		`<worksheet xmlns="` + spreadsheetNS + `" xmlns:a="urn:a" xmlns:a="urn:b"/>`,
		`<worksheet xmlns="` + spreadsheetNS + `" xmlns:xml="urn:wrong"/>`,
		`<worksheet xmlns="` + spreadsheetNS + `"><bad:sheetProtection/></worksheet>`,
		`<worksheet xmlns="` + spreadsheetNS + `" bad:attr="1"/>`,
		`<worksheet xmlns="urn:wrong"><sheetProtection/></worksheet>`,
		`<worksheet xmlns="` + spreadsheetNS + `"><sheetProtection>&unknown;</sheetProtection></worksheet>`,
		`<worksheet xmlns="` + spreadsheetNS + `"><!--bad--comment--></worksheet>`,
		`<worksheet xmlns="` + spreadsheetNS + `">` + string([]byte{0xff}) + `</worksheet>`,
		`<worksheet xmlns="` + spreadsheetNS + `"/><?xml version="1.0"?>`,
		"\u00a0" + `<worksheet xmlns="` + spreadsheetNS + `"/>`,
		`<![CDATA[ ]]><worksheet xmlns="` + spreadsheetNS + `"/>`,
	} {
		t.Run(fmt.Sprintf("%x", []byte(document)), func(t *testing.T) {
			if _, _, err := protectionXML(context.Background(), []byte(document), "worksheet"); err == nil {
				t.Fatalf("accepted invalid XML: %s", document)
			}
		})
	}
}

func TestRejectInvalidPackages(t *testing.T) {
	tests := []struct {
		name string
		edit func(map[string][]byte)
		want string
	}{
		{"missing-content-types", func(p map[string][]byte) { delete(p, "[Content_Types].xml") }, "Content_Types"},
		{"malformed-content-types", func(p map[string][]byte) { p["[Content_Types].xml"] = []byte("<") }, "content types"},
		{"wrong-content-namespace", func(p map[string][]byte) {
			p["[Content_Types].xml"] = bytes.ReplaceAll(p["[Content_Types].xml"], []byte(contentNS), []byte("urn:wrong"))
		}, "content type"},
		{"missing-root-rels", func(p map[string][]byte) { delete(p, "_rels/.rels") }, "officeDocument"},
		{"wrong-rels-namespace", func(p map[string][]byte) {
			p["_rels/.rels"] = bytes.ReplaceAll(p["_rels/.rels"], []byte(packageRelNS), []byte("urn:wrong"))
		}, "relationships"},
		{"missing-target", func(p map[string][]byte) { delete(p, "sheets/real.dat") }, "target"},
		{"wrong-sheet-type", func(p map[string][]byte) {
			p["[Content_Types].xml"] = bytes.ReplaceAll(p["[Content_Types].xml"], []byte(worksheetType), []byte("application/xml"))
		}, "content type"},
		{"wrong-workbook-type", func(p map[string][]byte) {
			p["[Content_Types].xml"] = bytes.ReplaceAll(p["[Content_Types].xml"], []byte(workbookType), []byte("application/xml"))
		}, "workbook content type"},
		{"workbook-namespace", func(p map[string][]byte) {
			p["book/main.xml"] = bytes.ReplaceAll(p["book/main.xml"], []byte(spreadsheetNS), []byte("urn:wrong"))
		}, "namespace"},
		{"worksheet-namespace", func(p map[string][]byte) {
			p["sheets/real.dat"] = bytes.ReplaceAll(p["sheets/real.dat"], []byte(spreadsheetNS), []byte("urn:wrong"))
		}, "namespace"},
		{"malformed-unrelated", func(p map[string][]byte) { p["unrelated.xml"] = []byte("<broken>") }, "XML"},
		{"trailing-root", func(p map[string][]byte) { p["book/main.xml"] = append(p["book/main.xml"], []byte("<extra/>")...) }, "XML"},
		{"signed-path", func(p map[string][]byte) { p["_xmlsignatures/sig1.xml"] = []byte("<signature/>") }, "signed"},
		{"signed-content-type", func(p map[string][]byte) {
			p["[Content_Types].xml"] = bytes.Replace(p["[Content_Types].xml"], []byte("</Types>"), []byte(`<Override PartName="/signature.xml" ContentType="application/vnd.openxmlformats-package.digital-signature-xmlsignature+xml"/></Types>`), 1)
		}, "signed"},
		{"signed-relationship", func(p map[string][]byte) {
			p["_rels/.rels"] = bytes.Replace(p["_rels/.rels"], []byte("/officeDocument"), []byte("/digital-signature/origin"), 1)
		}, "signed"},
		{"traversal", func(p map[string][]byte) { p["../evil.xml"] = []byte("<x/>") }, "entry name"},
		{"absolute-entry", func(p map[string][]byte) { p["/evil.xml"] = []byte("<x/>") }, "entry name"},
		{"backslash-entry", func(p map[string][]byte) { p[`bad\evil.xml`] = []byte("<x/>") }, "entry name"},
		{"duplicate-case", func(p map[string][]byte) { p["BOOK/main.xml"] = p["book/main.xml"] }, "duplicate"},
		{"external-workbook", func(p map[string][]byte) {
			p["_rels/.rels"] = bytes.Replace(p["_rels/.rels"], []byte(`Target="book/main.xml"`), []byte(`Target="https://example.invalid" TargetMode="External"`), 1)
		}, "officeDocument"},
		{"escaped-relationship", func(p map[string][]byte) {
			p["book/_rels/main.xml.rels"] = bytes.Replace(p["book/_rels/main.xml.rels"], []byte("../sheets/real.dat"), []byte("../../../../evil"), 1)
		}, "escapes"},
		{"duplicate-relationship-id", func(p map[string][]byte) {
			p["book/_rels/main.xml.rels"] = bytes.ReplaceAll(p["book/_rels/main.xml.rels"], []byte(`Id="web"`), []byte(`Id="sheet1"`))
		}, "duplicate"},
		{"missing-sheet-id", func(p map[string][]byte) {
			p["book/main.xml"] = bytes.ReplaceAll(p["book/main.xml"], []byte(`r:id="sheet1"`), []byte(`r:id="missing"`))
		}, "sheet"},
		{"duplicate-default", func(p map[string][]byte) {
			p["[Content_Types].xml"] = bytes.Replace(p["[Content_Types].xml"], []byte("</Types>"), []byte(`<Default Extension="xml" ContentType="application/xml"/></Types>`), 1)
		}, "duplicate"},
		{"duplicate-override", func(p map[string][]byte) {
			p["[Content_Types].xml"] = bytes.Replace(p["[Content_Types].xml"], []byte("</Types>"), []byte(`<Override PartName="/book/main.xml" ContentType="application/xml"/></Types>`), 1)
		}, "duplicate"},
		{"no-sheet-declarations", func(p map[string][]byte) {
			p["book/main.xml"] = []byte(`<workbook xmlns="` + spreadsheetNS + `"/>`)
		}, "no sheets"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := testDirectory(t)
			parts := fixture(true, 1, false)
			tc.edit(parts)
			input := filepath.Join(dir, "bad.xlsx")
			writeBytes(t, input, archiveBytes(t, parts, ""))
			if _, err := Inspect(context.Background(), input); err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
				t.Fatalf("error = %v; want %q", err, tc.want)
			}
			output := filepath.Join(dir, "out.xlsx")
			if _, err := Unlock(context.Background(), input, output, false, nil); err == nil {
				t.Fatal("Unlock accepted invalid package")
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("invalid package created output")
			}
			assertNoTemps(t, dir)
		})
	}
}

func TestSignaturesArchivesAndEncryption(t *testing.T) {
	dir := testDirectory(t)
	valid := archiveBytes(t, fixture(true, 1, false), "")
	encrypted := append([]byte(nil), valid...)
	mutateHeaders(encrypted, func(signature uint32, header []byte) {
		if signature == 0x04034b50 {
			binary.LittleEndian.PutUint16(header[6:], binary.LittleEndian.Uint16(header[6:])|1)
		} else {
			binary.LittleEndian.PutUint16(header[8:], binary.LittleEndian.Uint16(header[8:])|1)
		}
	})
	for _, tc := range []struct {
		name, ext, want string
		data            []byte
	}{
		{"ole-legacy", ".xls", "may be legacy", []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1, 0, 0}},
		{"ole-encrypted", ".xlsx", "password-to-open", []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}},
		{"encrypted-zip", ".xlsx", "encrypted ZIP", encrypted},
		{"not-zip", ".xlsx", "ZIP", []byte("not a zip")},
		{"short", ".xlsx", "ZIP", []byte("PK")},
		{"truncated", ".xlsx", "invalid ZIP", valid[:len(valid)/2]},
		{"wrong-extension", ".xls", "unsupported format", valid},
		{"macro-extension-mismatch", ".xlsm", "does not match", valid},
		{"duplicate", ".xlsx", "duplicate", archiveBytes(t, fixture(true, 1, false), "book/main.xml")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := filepath.Join(dir, tc.name+tc.ext)
			writeBytes(t, input, tc.data)
			if _, err := Inspect(context.Background(), input); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v; want %q", err, tc.want)
			}
		})
	}
	if Limitation != "This tool removes worksheet and workbook protection. It cannot remove password-to-open encryption. Encrypted files require the password in Excel." {
		t.Fatal("limitation text changed")
	}
}

func mutateHeaders(data []byte, edit func(uint32, []byte)) {
	for i := 0; i+46 <= len(data); i++ {
		signature := binary.LittleEndian.Uint32(data[i:])
		if signature == 0x04034b50 || signature == 0x02014b50 {
			edit(signature, data[i:])
		}
	}
}

func TestResourceLimits(t *testing.T) {
	dir := testDirectory(t)
	valid := archiveBytes(t, fixture(true, 1, false), "")
	t.Run("entry-size", func(t *testing.T) {
		data := append([]byte(nil), valid...)
		mutateHeaders(data, func(sig uint32, h []byte) {
			if sig == 0x02014b50 {
				binary.LittleEndian.PutUint32(h[24:], maxEntryBytes+1)
			}
		})
		assertLimit(t, dir, "entry", data, "64 MiB")
	})
	t.Run("total-expanded", func(t *testing.T) {
		parts := fixture(true, 1, false)
		for i := 0; i < 10; i++ {
			parts[fmt.Sprintf("extra%d.bin", i)] = []byte("x")
		}
		data := archiveBytes(t, parts, "")
		mutateHeaders(data, func(sig uint32, h []byte) {
			if sig == 0x02014b50 {
				binary.LittleEndian.PutUint32(h[24:], maxEntryBytes)
			}
		})
		assertLimit(t, dir, "total", data, "512 MiB")
	})
	t.Run("entry-count", func(t *testing.T) {
		parts := make(map[string][]byte)
		for i := 0; i <= maxEntries; i++ {
			parts[fmt.Sprintf("part%d.bin", i)] = nil
		}
		assertLimit(t, dir, "count", archiveBytes(t, parts, ""), "10000")
	})
	t.Run("xml-size", func(t *testing.T) {
		parts := fixture(true, 1, false)
		parts["large.xml"] = append([]byte("<x>"), bytes.Repeat([]byte(" "), maxXMLBytes)...)
		parts["large.xml"] = append(parts["large.xml"], []byte("</x>")...)
		assertLimit(t, dir, "xml", archiveBytes(t, parts, ""), "16 MiB")
	})
	t.Run("archive-size", func(t *testing.T) {
		input := filepath.Join(dir, "huge.xlsx")
		writeBytes(t, input, valid)
		if err := os.Truncate(input, maxArchiveBytes+1); err != nil {
			t.Fatal(err)
		}
		if _, err := Inspect(context.Background(), input); err == nil || !strings.Contains(err.Error(), "256 MiB") {
			t.Fatalf("error = %v", err)
		}
		os.Remove(input)
	})
	t.Run("xml-depth", func(t *testing.T) {
		data := []byte(strings.Repeat("<x>", maxXMLDepth+1) + strings.Repeat("</x>", maxXMLDepth+1))
		if _, err := scanXML(context.Background(), data, nil); err == nil || !strings.Contains(err.Error(), "nesting") {
			t.Fatalf("error = %v", err)
		}
	})
}

func assertLimit(t *testing.T, dir, name string, data []byte, message string) {
	t.Helper()
	input := filepath.Join(dir, name+".xlsx")
	writeBytes(t, input, data)
	if _, err := Inspect(context.Background(), input); err == nil || !strings.Contains(err.Error(), message) {
		t.Fatalf("error = %v; want %q", err, message)
	}
}

func TestOutputCollisionFailuresAndCleanup(t *testing.T) {
	for _, name := range []string{"same-path", "hardlink", "symlink", "input-symlink", "parent-symlink", "existing", "directory", "missing-parent", "wrong-extension", "empty"} {
		t.Run(name, func(t *testing.T) {
			dir := testDirectory(t)
			input := filepath.Join(dir, "source.xlsx")
			output := filepath.Join(dir, "result.xlsx")
			original := archiveBytes(t, fixture(true, 1, false), "")
			writeBytes(t, input, original)
			replace := true
			switch name {
			case "same-path":
				output = input
			case "hardlink":
				if err := os.Link(input, output); err != nil {
					t.Skip(err)
				}
			case "symlink":
				absolute, _ := filepath.Abs(input)
				if err := os.Symlink(absolute, output); err != nil {
					t.Skip(err)
				}
			case "input-symlink":
				alias := filepath.Join(dir, "alias.xlsx")
				absolute, _ := filepath.Abs(input)
				if err := os.Symlink(absolute, alias); err != nil {
					t.Skip(err)
				}
				input = alias
			case "parent-symlink":
				link := filepath.Join(dir, "parent")
				absolute, _ := filepath.Abs(dir)
				if err := os.Symlink(absolute, link); err != nil {
					t.Skip(err)
				}
				output = filepath.Join(link, "source.xlsx")
			case "existing":
				writeBytes(t, output, []byte("existing"))
				replace = false
			case "directory":
				if err := os.Mkdir(output, 0700); err != nil {
					t.Fatal(err)
				}
			case "missing-parent":
				output = filepath.Join(dir, "missing", "result.xlsx")
			case "wrong-extension":
				output = filepath.Join(dir, "result.xlsm")
			case "empty":
				output = ""
			}
			if _, err := Unlock(context.Background(), input, output, replace, nil); err == nil {
				t.Fatal("expected failure")
			}
			if !bytes.Equal(readBytes(t, input), original) {
				t.Fatal("input changed")
			}
			if name == "existing" && string(readBytes(t, output)) != "existing" {
				t.Fatal("existing destination changed")
			}
			assertNoTemps(t, dir)
		})
	}
}

func TestCancellationAndPublicationRaces(t *testing.T) {
	for _, stage := range []string{"before", "Inspecting workbook", "Removing protection", "Validating output", "Saving unlocked workbook"} {
		t.Run("cancel-"+stage, func(t *testing.T) {
			dir := testDirectory(t)
			input, output := filepath.Join(dir, "in.xlsx"), filepath.Join(dir, "out.xlsx")
			writeBytes(t, input, archiveBytes(t, fixture(true, 1, false), ""))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if stage == "before" {
				cancel()
			}
			_, err := Unlock(ctx, input, output, false, func(s string) {
				if s == stage {
					cancel()
				}
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v", err)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("cancellation published output")
			}
			assertNoTemps(t, dir)
		})
	}
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("destination-race-replace-%t", replace), func(t *testing.T) {
			dir := testDirectory(t)
			input, output := filepath.Join(dir, "in.xlsx"), filepath.Join(dir, "out.xlsx")
			writeBytes(t, input, archiveBytes(t, fixture(true, 1, false), ""))
			_, err := Unlock(context.Background(), input, output, replace, func(s string) {
				if s == "Saving unlocked workbook" {
					writeBytes(t, output, []byte("competitor"))
				}
			})
			if replace {
				if err != nil {
					t.Fatal(err)
				}
				if _, err = Inspect(context.Background(), output); err != nil {
					t.Fatal(err)
				}
			} else if err == nil || string(readBytes(t, output)) != "competitor" {
				t.Fatalf("overwrote racing destination: %v", err)
			}
			assertNoTemps(t, dir)
		})
	}
	t.Run("late-hardlink", func(t *testing.T) {
		dir := testDirectory(t)
		input, output := filepath.Join(dir, "in.xlsx"), filepath.Join(dir, "out.xlsx")
		before := archiveBytes(t, fixture(true, 1, false), "")
		writeBytes(t, input, before)
		_, err := Unlock(context.Background(), input, output, true, func(s string) {
			if s == "Saving unlocked workbook" {
				if err := os.Link(input, output); err != nil {
					t.Fatal(err)
				}
			}
		})
		if err == nil || !bytes.Equal(readBytes(t, input), before) {
			t.Fatalf("late collision not rejected: %v", err)
		}
		assertNoTemps(t, dir)
	})
}

func TestFailedValidationLeavesExistingOutputUntouched(t *testing.T) {
	dir := testDirectory(t)
	in, out := filepath.Join(dir, "in.xlsx"), filepath.Join(dir, "out.xlsx")
	original := archiveBytes(t, fixture(true, 1, false), "")
	writeBytes(t, in, original)
	writeBytes(t, out, []byte("keep existing destination"))
	_, err := Unlock(context.Background(), in, out, true, func(stage string) {
		if stage == "Validating output" {
			temps, e := filepath.Glob(filepath.Join(dir, ".excel-unlock-*"))
			if e != nil || len(temps) != 1 {
				t.Fatalf("temporary output = %v, %v", temps, e)
			}
			writeBytes(t, temps[0], []byte("damaged output"))
		}
	})
	if err == nil || !strings.Contains(err.Error(), "validate output") {
		t.Fatalf("error = %v", err)
	}
	if !bytes.Equal(readBytes(t, in), original) || string(readBytes(t, out)) != "keep existing destination" {
		t.Fatal("failure changed input or destination")
	}
	assertNoTemps(t, dir)
}

func TestInputReplacementAndMutationDuringOperation(t *testing.T) {
	for _, stage := range []string{"Removing protection", "Saving unlocked workbook"} {
		t.Run("replacement-"+stage, func(t *testing.T) {
			dir := testDirectory(t)
			input, output := filepath.Join(dir, "in.xlsx"), filepath.Join(dir, "out.xlsx")
			saved := filepath.Join(dir, "saved.xlsx")
			original := archiveBytes(t, fixture(true, 1, false), "")
			writeBytes(t, input, original)
			_, err := Unlock(context.Background(), input, output, true, func(s string) {
				if s == stage {
					if e := os.Rename(input, saved); e != nil {
						t.Skip(e)
					}
					writeBytes(t, input, []byte("replacement input must not be touched"))
					if e := os.Link(input, output); e != nil {
						t.Fatal(e)
					}
				}
			})
			if err == nil || !strings.Contains(err.Error(), "input file was replaced") {
				t.Fatalf("error = %v", err)
			}
			if !bytes.Equal(readBytes(t, saved), original) ||
				string(readBytes(t, input)) != "replacement input must not be touched" ||
				string(readBytes(t, output)) != "replacement input must not be touched" {
				t.Fatal("operation modified held input or replacement")
			}
			assertNoTemps(t, dir)
		})
	}
	t.Run("in-place-mutation", func(t *testing.T) {
		dir := testDirectory(t)
		input, output := filepath.Join(dir, "in.xlsx"), filepath.Join(dir, "out.xlsx")
		writeBytes(t, input, archiveBytes(t, fixture(true, 1, false), ""))
		_, err := Unlock(context.Background(), input, output, false, func(s string) {
			if s == "Saving unlocked workbook" {
				file, e := os.OpenFile(input, os.O_APPEND|os.O_WRONLY, 0600)
				if e != nil {
					t.Fatal(e)
				}
				_, e = file.WriteString("concurrent change")
				file.Close()
				if e != nil {
					t.Fatal(e)
				}
			}
		})
		if err == nil || !strings.Contains(err.Error(), "input contents changed") {
			t.Fatalf("error = %v", err)
		}
		if _, e := os.Stat(output); !os.IsNotExist(e) {
			t.Fatal("mutated input was published")
		}
		assertNoTemps(t, dir)
	})
}

func TestConcurrentNoClobberPublication(t *testing.T) {
	dir := testDirectory(t)
	output := filepath.Join(dir, "shared.xlsx")
	const workers = 16
	inputs := make([]string, workers)
	for i := range inputs {
		parts := fixture(true, 1, false)
		parts["assets/opaque.bin"] = []byte(fmt.Sprintf("unique caller %d", i))
		inputs[i] = filepath.Join(dir, fmt.Sprintf("in%d.xlsx", i))
		writeBytes(t, inputs[i], archiveBytes(t, parts, ""))
	}
	start := make(chan struct{})
	results := make(chan error, workers)
	var wait sync.WaitGroup
	for _, input := range inputs {
		wait.Add(1)
		go func(input string) {
			defer wait.Done()
			<-start
			_, err := Unlock(context.Background(), input, output, false, nil)
			results <- err
		}(input)
	}
	close(start)
	wait.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("successful publishers = %d; want exactly one", success)
	}
	if info, err := Inspect(context.Background(), output); err != nil || info.Protected() {
		t.Fatalf("published output = %+v, %v", info, err)
	}
	assertNoTemps(t, dir)
}

func TestAtomicPublicationPrimitive(t *testing.T) {
	dir := testDirectory(t)
	output := filepath.Join(dir, "destination")
	const workers = 16
	start := make(chan struct{})
	type result struct {
		id  int
		err error
	}
	results := make(chan result, workers)
	for id := 0; id < workers; id++ {
		source := filepath.Join(dir, fmt.Sprintf("source-%d", id))
		writeBytes(t, source, []byte(fmt.Sprintf("caller-%d", id)))
		go func(id int, source string) {
			<-start
			results <- result{id, publishNewFile(source, output)}
		}(id, source)
	}
	close(start)
	success, winner := 0, -1
	for id := 0; id < workers; id++ {
		result := <-results
		if result.err == nil {
			success++
			winner = result.id
		}
	}
	if success != 1 {
		t.Fatalf("successful atomic publishers = %d; want one", success)
	}
	if string(readBytes(t, output)) != fmt.Sprintf("caller-%d", winner) {
		t.Fatal("racing publication clobbered the winner")
	}
}

func TestChecksumAndCompressionValidation(t *testing.T) {
	dir := testDirectory(t)
	valid := archiveBytes(t, fixture(true, 1, false), "")
	for _, kind := range []string{"checksum", "compression"} {
		t.Run(kind, func(t *testing.T) {
			data := append([]byte(nil), valid...)
			mutateHeaders(data, func(sig uint32, header []byte) {
				if sig != 0x02014b50 {
					return
				}
				if kind == "checksum" {
					header[16] ^= 0xff
				} else {
					binary.LittleEndian.PutUint16(header[10:], 99)
				}
			})
			input := filepath.Join(dir, kind+".xlsx")
			writeBytes(t, input, data)
			if _, err := Inspect(context.Background(), input); err == nil || !strings.Contains(strings.ToLower(err.Error()), kind) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestPartTargetResolution(t *testing.T) {
	for _, tc := range []struct{ source, target, want string }{
		{"book/main.xml", "../sheets/real.dat", "sheets/real.dat"},
		{"book/main.xml", "/sheets/real.dat", "sheets/real.dat"},
		{"book/main.xml", "../sheets/real%20sheet.xml", "sheets/real sheet.xml"},
		{"book/main.xml", "#anchor", "book/main.xml"},
		{"", "book/main.xml", "book/main.xml"},
	} {
		got, err := resolveTarget(tc.source, tc.target)
		if err != nil || got != tc.want {
			t.Fatalf("resolve %q = %q, %v; want %q", tc.target, got, err, tc.want)
		}
	}
	for _, target := range []string{"../../evil", "/../../evil", "%2e%2e/%2e%2e/evil", "http://example.invalid/x", "//host/x", `bad\x`, "bad?query=1", "%00", "%zz", ""} {
		if _, err := resolveTarget("", target); err == nil {
			t.Fatalf("accepted invalid target %q", target)
		}
	}
}

func TestOverwriteAndCaseInsensitiveExtensions(t *testing.T) {
	dir := testDirectory(t)
	input, output := filepath.Join(dir, "in.XLSX"), filepath.Join(dir, "out.xlsx")
	writeBytes(t, input, archiveBytes(t, fixture(true, 1, false), ""))
	writeBytes(t, output, []byte("old contents"))
	if _, err := Unlock(context.Background(), input, output, true, nil); err != nil {
		t.Fatal(err)
	}
	info, err := Inspect(context.Background(), output)
	if err != nil || info.Protected() {
		t.Fatalf("output = %+v, %v", info, err)
	}
	assertNoTemps(t, dir)
}

func utf16XML(document string, order binary.ByteOrder) []byte {
	data := make([]byte, 2)
	order.PutUint16(data, 0xfeff)
	for _, unit := range utf16.Encode([]rune(document)) {
		var encoded [2]byte
		order.PutUint16(encoded[:], unit)
		data = append(data, encoded[:]...)
	}
	return data
}

func TestXMLByteOrderMarksAndUTF16(t *testing.T) {
	document := `<?xml version="1.0" encoding="UTF-16"?><worksheet xmlns="` + spreadsheetNS + `"><sheetData><v>日本語 😀 &amp; text</v></sheetData><!-- keep --><sheetProtection password = 'ABCD' /><sheetProtection></sheetProtection></worksheet>`
	want := strings.ReplaceAll(document, `<sheetProtection password = 'ABCD' /><sheetProtection></sheetProtection>`, "")
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		data := utf16XML(document, order)
		edited, count, err := protectionXML(context.Background(), data, "worksheet")
		if err != nil || count != 2 || !bytes.Equal(edited, utf16XML(want, order)) {
			t.Fatalf("UTF-16 %v edit failed: count %d, error %v", order, count, err)
		}
		if _, n, err := protectionXML(context.Background(), edited, "worksheet"); err != nil || n != 0 {
			t.Fatalf("UTF-16 %v result invalid: %d, %v", order, n, err)
		}
	}
	utf8Document := strings.ReplaceAll(document, "UTF-16", "UTF-8")
	utf8Want := strings.ReplaceAll(want, "UTF-16", "UTF-8")
	bom := []byte{0xef, 0xbb, 0xbf}
	edited, count, err := protectionXML(context.Background(), append(bom, []byte(utf8Document)...), "worksheet")
	if err != nil || count != 2 || !bytes.Equal(edited, append(bom, []byte(utf8Want)...)) {
		t.Fatalf("UTF-8 BOM edit failed: %d, %v", count, err)
	}
	for _, bad := range [][]byte{
		{0xff, 0xfe, 0x00},
		{0xff, 0xfe, 0x00, 0xd8},
		{0xff, 0xfe, 0x00, 0xdc},
		{0xff, 0xfe, 0x00, 0xd8, 0x01, 0x00},
	} {
		if _, _, err := protectionXML(context.Background(), bad, "worksheet"); err == nil {
			t.Fatalf("accepted malformed UTF-16: %x", bad)
		}
	}
	dir := testDirectory(t)
	for i, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		parts := fixture(true, 1, false)
		for name, data := range parts {
			if isXML(name, "") || name == "sheets/real.dat" {
				text := strings.ReplaceAll(string(data), `encoding="UTF-8"`, `encoding="UTF-16"`)
				text = strings.ReplaceAll(text, `<?xml version="1.0"?>`, `<?xml version="1.0" encoding="UTF-16"?>`)
				parts[name] = utf16XML(text, order)
			}
		}
		in, out := filepath.Join(dir, fmt.Sprintf("in%d.xlsx", i)), filepath.Join(dir, fmt.Sprintf("out%d.xlsx", i))
		writeBytes(t, in, archiveBytes(t, parts, ""))
		if _, err := Unlock(context.Background(), in, out, false, nil); err != nil {
			t.Fatalf("UTF-16 package: %v", err)
		}
	}
}

func TestXMLAttributeAndNamespaceLimits(t *testing.T) {
	var attributes, namespaces strings.Builder
	for i := 0; i <= maxXMLAttrs; i++ {
		fmt.Fprintf(&attributes, ` a%d="v"`, i)
	}
	for i := 0; i <= maxXMLNamespaces; i++ {
		fmt.Fprintf(&namespaces, ` xmlns:a%d="urn:a%d"`, i, i)
	}
	for _, text := range []string{"<x" + attributes.String() + "/>", "<x" + namespaces.String() + "/>"} {
		if _, err := scanXML(context.Background(), []byte(text), nil); err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("limit error = %v", err)
		}
	}
}

func TestStrictAndPrefixedPackages(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(fmt.Sprint(strict), func(t *testing.T) {
			dir := testDirectory(t)
			parts := fixture(true, 1, false)
			for name, data := range parts {
				if strict {
					data = bytes.ReplaceAll(data, []byte(spreadsheetNS), []byte(strictSheetNS))
					data = bytes.ReplaceAll(data, []byte(officeRelNS), []byte(strictRelNS))
				}
				if name == "sheets/real.dat" {
					data = bytes.ReplaceAll(data, []byte(`xmlns="`), []byte(`xmlns:s="`))
					for _, local := range []string{"worksheet", "sheetData", "row", "c", "is", "t", "sheetProtection", "extLst"} {
						for _, suffix := range []string{">", " ", "/"} {
							data = bytes.ReplaceAll(data, []byte("<"+local+suffix), []byte("<s:"+local+suffix))
						}
						data = bytes.ReplaceAll(data, []byte("</"+local+">"), []byte("</s:"+local+">"))
					}
				}
				parts[name] = data
			}
			input, output := filepath.Join(dir, "in.xlsx"), filepath.Join(dir, "out.xlsx")
			writeBytes(t, input, archiveBytes(t, parts, ""))
			info, err := Unlock(context.Background(), input, output, false, nil)
			if err != nil || !reflect.DeepEqual(info, Info{Sheets: 1, Workbook: true}) {
				t.Fatalf("Unlock = %+v, %v", info, err)
			}
		})
	}
}
