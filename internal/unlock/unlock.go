// Package unlock removes editing protection from unencrypted OOXML workbooks.
package unlock

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const Limitation = "This tool removes worksheet and workbook protection. It cannot remove password-to-open encryption. Encrypted files require the password in Excel."

// Resource ceilings apply to both inspection and unlocking. XML parts have a
// tighter ceiling because surgical editing requires holding one part in memory.
const (
	maxArchiveBytes  = 256 << 20
	maxEntryBytes    = 64 << 20
	maxTotalBytes    = 512 << 20
	maxXMLBytes      = 16 << 20
	maxEntries       = 10000
	maxXMLDepth      = 256
	maxXMLAttrs      = 1024
	maxXMLNamespaces = 256
)

const (
	contentNS     = "http://schemas.openxmlformats.org/package/2006/content-types"
	packageRelNS  = "http://schemas.openxmlformats.org/package/2006/relationships"
	spreadsheetNS = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	strictSheetNS = "http://purl.oclc.org/ooxml/spreadsheetml/main"
	officeRelNS   = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	strictRelNS   = "http://purl.oclc.org/ooxml/officeDocument/relationships"
	workbookType  = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"
	macroBookType = "application/vnd.ms-excel.sheet.macroEnabled.main+xml"
	worksheetType = "application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"
)

type Info struct {
	Sheets       int
	Workbook     bool
	MacroEnabled bool
}

func (i Info) Protected() bool { return i.Sheets > 0 || i.Workbook }

type packageFile struct {
	file       *os.File
	source     os.FileInfo
	zip        *zip.Reader
	entries    map[string]*zip.File
	types      contentTypes
	workbook   string
	worksheets map[string]bool
	info       Info
}

type contentTypes struct {
	defaults  map[string]string
	overrides map[string]string
}

func (t contentTypes) get(name string) string {
	if v := t.overrides[name]; v != "" {
		return v
	}
	return t.defaults[strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))]
}

type relationship struct {
	id, kind, target string
	external         bool
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Inspect validates the entire package without modifying it. XML parts may use
// UTF-8 (with or without a BOM) or BOM-marked UTF-16LE/UTF-16BE. Oversized XML,
// excessive nesting, attributes, and namespace bindings are refused.
func Inspect(ctx context.Context, filename string) (Info, error) {
	p, err := inspect(ctx, filename)
	if err != nil {
		return Info{}, err
	}
	defer p.file.Close()
	return p.info, nil
}

// Unlock returns the original protection counts. It publishes only a fully
// validated result, and never modifies input. replace permits replacing an
// existing regular destination; it does not permit replacing the input itself.
func Unlock(ctx context.Context, input, output string, replace bool, progress func(stage string)) (Info, error) {
	report := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	report("Inspecting workbook")
	p, err := inspect(ctx, input)
	if err != nil {
		return Info{}, err
	}
	defer p.file.Close()
	if !p.info.Protected() {
		return p.info, errors.New("workbook has no worksheet or workbook protection to remove")
	}
	destination, err := outputPath(input, output, p.file, replace)
	if err != nil {
		return p.info, err
	}
	// Match the input's extension so the validation pass enforces the same
	// workbook format even when the caller uses a different output filename.
	ext := strings.ToLower(filepath.Ext(input))
	if !strings.EqualFold(filepath.Ext(destination), ext) {
		return p.info, errors.New("output extension must match the input (.xlsx or .xlsm)")
	}
	temp, err := os.CreateTemp(filepath.Dir(destination), ".excel-unlock-*"+ext)
	if err != nil {
		return p.info, fmt.Errorf("create output: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	defer temp.Close()
	report("Removing protection")
	writer := zip.NewWriter(temp)
	if err = writer.SetComment(p.zip.Comment); err != nil {
		return p.info, err
	}
	for _, entry := range p.zip.File {
		if err = ctx.Err(); err != nil {
			return p.info, err
		}
		kind := ""
		if entry.Name == p.workbook {
			kind = "workbook"
		} else if p.worksheets[entry.Name] {
			kind = "worksheet"
		}
		if kind == "" {
			err = writer.Copy(entry)
		} else {
			var data, edited []byte
			data, err = readPart(ctx, entry, maxXMLBytes)
			if err == nil {
				edited, _, err = protectionXML(ctx, data, kind)
			}
			if err == nil {
				if bytes.Equal(data, edited) {
					err = writer.Copy(entry)
				} else {
					header := entry.FileHeader
					// Keep DOS timestamps and existing extra fields verbatim;
					// CreateHeader otherwise appends a second timestamp field.
					header.Modified = time.Time{}
					var out io.Writer
					out, err = writer.CreateHeader(&header)
					if err == nil {
						_, err = out.Write(edited)
					}
				}
			}
		}
		if err != nil {
			return p.info, fmt.Errorf("write %s: %w", entry.Name, err)
		}
	}
	if err = writer.Close(); err != nil {
		return p.info, fmt.Errorf("finish ZIP: %w", err)
	}
	if err = temp.Sync(); err != nil {
		return p.info, fmt.Errorf("flush output: %w", err)
	}
	if err = temp.Close(); err != nil {
		return p.info, err
	}
	report("Validating output")
	result, err := Inspect(ctx, tempName)
	if err != nil {
		return p.info, fmt.Errorf("validate output: %w", err)
	}
	if result.Protected() || result.MacroEnabled != p.info.MacroEnabled {
		return p.info, errors.New("output validation failed: protection remains or workbook format changed")
	}
	report("Saving unlocked workbook")
	if err = ctx.Err(); err != nil {
		return p.info, err
	}
	if _, err = outputPath(input, destination, p.file, replace); err != nil {
		return p.info, err
	}
	if err = p.unchanged(); err != nil {
		return p.info, err
	}
	if err = ctx.Err(); err != nil {
		return p.info, err
	}
	if replace {
		err = replaceFile(tempName, destination)
	} else {
		err = publishNewFile(tempName, destination)
	}
	if err != nil {
		return p.info, fmt.Errorf("publish output: %w", err)
	}
	return p.info, nil
}

func outputPath(input, output string, opened *os.File, replace bool) (string, error) {
	if output == "" {
		return "", errors.New("output path is empty")
	}
	in, err := filepath.Abs(input)
	if err != nil {
		return "", err
	}
	in, err = filepath.EvalSymlinks(in)
	if err != nil {
		return "", fmt.Errorf("resolve input: %w", err)
	}
	out, err := filepath.Abs(output)
	if err != nil {
		return "", err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(out))
	if err != nil {
		return "", fmt.Errorf("resolve output directory: %w", err)
	}
	out = filepath.Join(dir, filepath.Base(out))
	if in == out {
		return "", errors.New("input and output must be different files")
	}
	sourceInfo, err := opened.Stat()
	if err != nil {
		return "", err
	}
	currentInput, err := os.Lstat(input)
	if err != nil {
		return "", fmt.Errorf("recheck input: %w", err)
	}
	if !currentInput.Mode().IsRegular() || !os.SameFile(sourceInfo, currentInput) {
		return "", errors.New("input file was replaced during the operation")
	}
	fi, err := os.Lstat(out)
	if err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("output must not be a symbolic link")
		}
		if os.SameFile(sourceInfo, fi) || os.SameFile(currentInput, fi) {
			return "", errors.New("input and output refer to the same file")
		}
		if !fi.Mode().IsRegular() {
			return "", errors.New("output must be a regular file")
		}
		if !replace {
			return "", errors.New("output already exists")
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return out, nil
}

func inspect(ctx context.Context, filename string) (_ *packageFile, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	lstat, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !lstat.Mode().IsRegular() {
		return nil, errors.New("input must be a regular file, not a symbolic link")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			file.Close()
		}
	}()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(lstat, stat) {
		return nil, errors.New("input changed while opening it")
	}
	var signature [8]byte
	n, readErr := file.ReadAt(signature[:], 0)
	if readErr != nil && readErr != io.EOF {
		return nil, readErr
	}
	if n == 8 && bytes.Equal(signature[:], []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}) {
		return nil, fmt.Errorf("OLE compound files are not supported: these may be legacy Excel workbooks or password-to-open encrypted files. %s", Limitation)
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if ext != ".xlsx" && ext != ".xlsm" {
		return nil, errors.New("unsupported format: choose an .xlsx or .xlsm workbook")
	}
	if n < 4 || !bytes.Equal(signature[:4], []byte{'P', 'K', 3, 4}) {
		return nil, errors.New("invalid workbook: expected an OOXML ZIP archive")
	}
	if stat.Size() > maxArchiveBytes {
		return nil, errors.New("archive exceeds the 256 MiB size limit")
	}
	reader, err := prepareZIPReader(ctx, file, stat.Size())
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(reader, reader.archiveSize)
	if err != nil {
		return nil, fmt.Errorf("invalid ZIP archive: %w", err)
	}
	reader.remaining = -1
	zr.Comment = reader.archiveComment
	if len(zr.File) > maxEntries {
		return nil, errors.New("archive exceeds the 10000-entry limit")
	}
	p := &packageFile{file: file, source: stat, zip: zr, entries: make(map[string]*zip.File), worksheets: make(map[string]bool)}
	seen := make(map[string]bool)
	var total uint64
	for _, entry := range zr.File {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name
		clean := strings.TrimSuffix(name, "/")
		if clean == "" || clean == ".." || strings.HasPrefix(clean, "../") ||
			strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") || path.Clean(clean) != clean {
			return nil, fmt.Errorf("invalid archive entry name: %q", name)
		}
		key := strings.ToLower(clean)
		if seen[key] {
			return nil, fmt.Errorf("duplicate archive entry: %s", name)
		}
		seen[key] = true
		if strings.HasPrefix(key, "_xmlsignatures/") || key == "_xmlsignatures" {
			return nil, errors.New("digitally signed packages are not supported")
		}
		if entry.Flags&(1|64) != 0 {
			return nil, fmt.Errorf("encrypted ZIP entries are not supported. %s", Limitation)
		}
		if entry.Method != zip.Store && entry.Method != zip.Deflate {
			return nil, errors.New("unsupported ZIP compression method")
		}
		if entry.UncompressedSize64 > maxEntryBytes {
			return nil, errors.New("archive entry exceeds the 64 MiB limit")
		}
		total += entry.UncompressedSize64
		if total > maxTotalBytes {
			return nil, errors.New("archive exceeds the 512 MiB expanded size limit")
		}
		p.entries[name] = entry
	}
	ct, err := p.read(ctx, "[Content_Types].xml")
	if err != nil {
		return nil, fmt.Errorf("missing or invalid [Content_Types].xml: %w", err)
	}
	p.types, err = parseContentTypes(ctx, ct)
	if err != nil {
		return nil, err
	}
	for name := range p.types.overrides {
		if p.entries[name] == nil {
			return nil, fmt.Errorf("content type override target is missing: %s", name)
		}
	}
	relsBySource := make(map[string][]relationship)
	hasVBA := false
	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		ct := p.types.get(entry.Name)
		if strings.EqualFold(ct, "application/vnd.ms-office.vbaProject") {
			hasVBA = true
		}
		if entry.Name != "[Content_Types].xml" && ct == "" {
			return nil, fmt.Errorf("missing content type for %s", entry.Name)
		}
		if strings.Contains(strings.ToLower(ct), "signature") {
			return nil, errors.New("digitally signed packages are not supported")
		}
		if isXML(entry.Name, ct) {
			data, e := readPart(ctx, entry, maxXMLBytes)
			if e != nil {
				return nil, e
			}
			if _, e = scanXML(ctx, data, nil); e != nil {
				return nil, fmt.Errorf("invalid XML in %s: %w", entry.Name, e)
			}
			if strings.HasSuffix(entry.Name, ".rels") {
				if ct != "application/vnd.openxmlformats-package.relationships+xml" {
					return nil, fmt.Errorf("invalid relationship content type for %s", entry.Name)
				}
				source, e := relationshipSource(entry.Name)
				if e != nil {
					return nil, e
				}
				if source != "" && p.entries[source] == nil {
					return nil, fmt.Errorf("relationships have no source part: %s", entry.Name)
				}
				rels, e := parseRelationships(ctx, data, source)
				if e != nil {
					return nil, fmt.Errorf("invalid relationships %s: %w", entry.Name, e)
				}
				for _, rel := range rels {
					if !rel.external && p.entries[rel.target] == nil {
						return nil, fmt.Errorf("relationship target is missing: %s", rel.target)
					}
				}
				relsBySource[source] = rels
			}
		} else {
			r, e := entry.Open()
			if e != nil {
				return nil, e
			}
			_, e = io.Copy(io.Discard, io.LimitReader(contextReader{ctx, r}, maxEntryBytes+1))
			closeErr := r.Close()
			if e != nil {
				return nil, fmt.Errorf("read %s: %w", entry.Name, e)
			}
			if closeErr != nil {
				return nil, closeErr
			}
		}
	}
	for _, rel := range relsBySource[""] {
		if officeRelationship(rel.kind, "officeDocument") {
			if p.workbook != "" || rel.external {
				return nil, errors.New("invalid officeDocument relationship")
			}
			p.workbook = rel.target
		}
	}
	if p.workbook == "" {
		return nil, errors.New("missing package officeDocument relationship")
	}
	mainType := p.types.get(p.workbook)
	if mainType != workbookType && mainType != macroBookType {
		return nil, errors.New("unsupported workbook content type")
	}
	p.info.MacroEnabled = mainType == macroBookType
	if hasVBA && !p.info.MacroEnabled {
		return nil, errors.New("VBA projects require an .xlsm macro-enabled workbook")
	}
	if (ext == ".xlsm") != p.info.MacroEnabled {
		return nil, errors.New("workbook content type does not match its filename extension")
	}
	workbookData, err := p.read(ctx, p.workbook)
	if err != nil {
		return nil, err
	}
	_, count, err := protectionXML(ctx, workbookData, "workbook")
	if err != nil {
		return nil, fmt.Errorf("invalid workbook XML: %w", err)
	}
	p.info.Workbook = count > 0
	byID := make(map[string]relationship)
	for _, rel := range relsBySource[p.workbook] {
		byID[rel.id] = rel
		if officeRelationship(rel.kind, "worksheet") {
			if rel.external || p.types.get(rel.target) != worksheetType {
				return nil, errors.New("worksheet relationship has an invalid target or content type")
			}
			p.worksheets[rel.target] = true
		}
	}
	sheetCount := 0
	inSheets := false
	seenSheetIDs := make(map[string]bool)
	_, err = scanXML(ctx, workbookData, func(tok xml.Token, depth int, _, _ int64) error {
		if end, ok := tok.(xml.EndElement); ok && depth == 2 && end.Name.Local == "sheets" {
			inSheets = false
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			return nil
		}
		if depth == 2 && start.Name.Local == "sheets" && isSpreadsheetNS(start.Name.Space) {
			inSheets = true
		}
		if inSheets && depth == 3 && start.Name.Local == "sheet" && isSpreadsheetNS(start.Name.Space) {
			id := ""
			for _, a := range start.Attr {
				if a.Name.Local == "id" && (a.Name.Space == officeRelNS || a.Name.Space == strictRelNS) {
					id = a.Value
				}
			}
			rel, ok := byID[id]
			if !ok || rel.external || seenSheetIDs[id] || !isSheetRelationship(rel.kind) {
				return errors.New("workbook sheet has no valid internal relationship")
			}
			seenSheetIDs[id] = true
			sheetCount++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if sheetCount == 0 {
		return nil, errors.New("workbook contains no sheets")
	}
	for name := range p.worksheets {
		data, e := p.read(ctx, name)
		if e != nil {
			return nil, e
		}
		_, count, e := protectionXML(ctx, data, "worksheet")
		if e != nil {
			return nil, fmt.Errorf("invalid worksheet %s: %w", name, e)
		}
		p.info.Sheets += count
	}
	if err = p.unchanged(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *packageFile) unchanged() error {
	current, err := p.file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(p.source, current) || current.Size() != p.source.Size() || !current.ModTime().Equal(p.source.ModTime()) {
		return errors.New("input contents changed during the operation")
	}
	return nil
}

func isXML(name, ct string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".xml") || strings.HasSuffix(name, ".rels") ||
		strings.HasSuffix(ct, "+xml") || ct == "application/xml" || ct == "text/xml"
}

func (p *packageFile) read(ctx context.Context, name string) ([]byte, error) {
	f := p.entries[name]
	if f == nil {
		return nil, fmt.Errorf("missing package part %s", name)
	}
	return readPart(ctx, f, maxXMLBytes)
}

func readPart(ctx context.Context, f *zip.File, limit int64) ([]byte, error) {
	if f.UncompressedSize64 > uint64(limit) {
		return nil, fmt.Errorf("XML part %s exceeds the 16 MiB limit", f.Name)
	}
	r, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, r}, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.Name, err)
	}
	if int64(len(data)) > limit {
		return nil, errors.New("expanded entry exceeds size limit")
	}
	return data, nil
}

func isSpreadsheetNS(ns string) bool { return ns == spreadsheetNS || ns == strictSheetNS }

func officeRelationship(kind, suffix string) bool {
	return kind == officeRelNS+"/"+suffix || kind == strictRelNS+"/"+suffix
}

func isSheetRelationship(kind string) bool {
	return officeRelationship(kind, "worksheet") || officeRelationship(kind, "chartsheet") ||
		officeRelationship(kind, "dialogsheet") ||
		kind == "http://schemas.microsoft.com/office/2006/relationships/xlMacrosheet" ||
		kind == "http://schemas.microsoft.com/office/2006/relationships/xlIntlMacrosheet"
}

func attr(start xml.StartElement, name string) string {
	for _, a := range start.Attr {
		if a.Name.Space == "" && a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func parseContentTypes(ctx context.Context, data []byte) (contentTypes, error) {
	types := contentTypes{defaults: make(map[string]string), overrides: make(map[string]string)}
	root, err := scanXML(ctx, data, func(tok xml.Token, depth int, _, _ int64) error {
		if text, ok := tok.(xml.CharData); ok && len(bytes.Trim(text, " \t\r\n")) != 0 {
			return errors.New("unexpected text in content types")
		}
		start, ok := tok.(xml.StartElement)
		if !ok || depth == 1 {
			return nil
		}
		if depth != 2 || start.Name.Space != contentNS {
			return errors.New("invalid content type declaration")
		}
		mime := attr(start, "ContentType")
		if mime == "" {
			return errors.New("empty content type")
		}
		if strings.Contains(strings.ToLower(mime), "signature") {
			return errors.New("digitally signed packages are not supported")
		}
		switch start.Name.Local {
		case "Default":
			ext := strings.ToLower(attr(start, "Extension"))
			if ext == "" || strings.ContainsAny(ext, "/\\.") || types.defaults[ext] != "" {
				return errors.New("invalid or duplicate default content type")
			}
			types.defaults[ext] = mime
		case "Override":
			name := attr(start, "PartName")
			if !strings.HasPrefix(name, "/") {
				return errors.New("content type override must be an absolute part name")
			}
			resolved, e := resolveTarget("", name)
			if e != nil || types.overrides[resolved] != "" {
				return errors.New("invalid or duplicate content type override")
			}
			types.overrides[resolved] = mime
		default:
			return errors.New("unknown content type declaration")
		}
		return nil
	})
	if err != nil {
		return types, fmt.Errorf("invalid content types: %w", err)
	}
	if root != (xml.Name{Space: contentNS, Local: "Types"}) {
		return types, errors.New("invalid content types root namespace or element")
	}
	return types, nil
}

func relationshipSource(name string) (string, error) {
	if name == "_rels/.rels" {
		return "", nil
	}
	dir, base := path.Split(name)
	if path.Base(strings.TrimSuffix(dir, "/")) != "_rels" || base == ".rels" {
		return "", fmt.Errorf("invalid relationship part name: %s", name)
	}
	return path.Join(path.Dir(strings.TrimSuffix(dir, "/")), strings.TrimSuffix(base, ".rels")), nil
}

func parseRelationships(ctx context.Context, data []byte, source string) ([]relationship, error) {
	var rels []relationship
	ids := make(map[string]bool)
	root, err := scanXML(ctx, data, func(tok xml.Token, depth int, _, _ int64) error {
		if text, ok := tok.(xml.CharData); ok && len(bytes.Trim(text, " \t\r\n")) != 0 {
			return errors.New("unexpected text in relationships")
		}
		start, ok := tok.(xml.StartElement)
		if !ok || depth == 1 {
			return nil
		}
		if depth != 2 || start.Name != (xml.Name{Space: packageRelNS, Local: "Relationship"}) {
			return errors.New("invalid relationship declaration")
		}
		rel := relationship{id: attr(start, "Id"), kind: attr(start, "Type"), target: attr(start, "Target")}
		if rel.id == "" || rel.kind == "" || rel.target == "" || ids[rel.id] {
			return errors.New("missing or duplicate relationship attributes")
		}
		ids[rel.id] = true
		if strings.Contains(strings.ToLower(rel.kind), "digital-signature") {
			return errors.New("digitally signed packages are not supported")
		}
		switch attr(start, "TargetMode") {
		case "", "Internal":
			var e error
			rel.target, e = resolveTarget(source, rel.target)
			if e != nil {
				return e
			}
		case "External":
			rel.external = true
		default:
			return errors.New("invalid relationship TargetMode")
		}
		rels = append(rels, rel)
		return nil
	})
	if err == nil && root != (xml.Name{Space: packageRelNS, Local: "Relationships"}) {
		err = errors.New("invalid relationships root namespace or element")
	}
	return rels, err
}

func resolveTarget(source, target string) (string, error) {
	u, err := url.Parse(target)
	if err != nil || u.IsAbs() || u.Host != "" || u.RawQuery != "" {
		return "", errors.New("invalid internal relationship target")
	}
	name := u.Path
	if strings.ContainsAny(name, "\\\x00") {
		return "", errors.New("invalid package part name")
	}
	if name == "" {
		name = source
	} else if !strings.HasPrefix(name, "/") {
		name = path.Dir(source) + "/" + name
	}
	var segments []string
	for _, segment := range strings.Split(name, "/") {
		switch segment {
		case "", ".":
		case "..":
			if len(segments) == 0 {
				return "", errors.New("relationship escapes the package")
			}
			segments = segments[:len(segments)-1]
		default:
			segments = append(segments, segment)
		}
	}
	name = strings.Join(segments, "/")
	if name == "" {
		return "", errors.New("relationship has an empty part name")
	}
	return name, nil
}
