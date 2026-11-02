package unlock

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

type xmlVisitor func(token xml.Token, depth int, start, end int64) error

// scanXML validates a single, namespace-well-formed XML document. RawToken is
// used to reject undeclared prefixes, something Decoder.Token alone permits.
// InputOffset is mapped back to original bytes, including for UTF-16 documents.
func scanXML(ctx context.Context, data []byte, visit xmlVisitor) (xml.Name, error) {
	text, utf16, offset, err := xmlText(data)
	if err != nil {
		return xml.Name{}, err
	}
	decoder := xml.NewDecoder(contextReader{ctx, bytes.NewReader(text)})
	if utf16 {
		decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
			if strings.EqualFold(charset, "UTF-16") || strings.EqualFold(charset, "UTF-16LE") || strings.EqualFold(charset, "UTF-16BE") {
				return input, nil // Already transcoded; offsets are mapped below.
			}
			return nil, fmt.Errorf("unsupported XML encoding %q", charset)
		}
	}
	var lastText int64
	lastRaw := offset
	rawOffset := func(position int64) int64 {
		if !utf16 {
			return position + offset
		}
		for _, r := range string(text[lastText:position]) {
			lastRaw += 2
			if r > 0xffff {
				lastRaw += 2
			}
		}
		lastText = position
		return lastRaw
	}
	type frame struct {
		raw xml.Name
		ns  map[string]string
	}
	var stack []frame
	var root xml.Name
	closed := false
	translate := func(name xml.Name, ns map[string]string, attribute bool) (xml.Name, error) {
		if name.Space == "" {
			if !attribute {
				name.Space = ns[""]
			}
			return name, nil
		}
		uri, exists := ns[name.Space]
		if !exists || uri == "" {
			return name, fmt.Errorf("undeclared XML namespace prefix %q", name.Space)
		}
		name.Space = uri
		return name, nil
	}
	for {
		textStart := decoder.InputOffset()
		start := rawOffset(textStart)
		tok, err := decoder.RawToken()
		if err == io.EOF {
			if len(stack) != 0 || root.Local == "" || !closed {
				return root, errors.New("incomplete XML document")
			}
			return root, nil
		}
		if err != nil {
			return root, err
		}
		if err = ctx.Err(); err != nil {
			return root, err
		}
		depth := len(stack)
		switch token := tok.(type) {
		case xml.StartElement:
			if closed {
				return root, errors.New("multiple XML roots")
			}
			if len(stack) >= maxXMLDepth {
				return root, errors.New("XML nesting exceeds the 256-element limit")
			}
			if len(token.Attr) > maxXMLAttrs {
				return root, errors.New("XML element exceeds the 1024-attribute limit")
			}
			ns := map[string]string{"xml": "http://www.w3.org/XML/1998/namespace"}
			if len(stack) > 0 {
				for k, v := range stack[len(stack)-1].ns {
					ns[k] = v
				}
			}
			declared := make(map[string]bool)
			for _, a := range token.Attr {
				if a.Name.Space == "xmlns" || (a.Name.Space == "" && a.Name.Local == "xmlns") {
					prefix := a.Name.Local
					if a.Name.Space == "" {
						prefix = ""
					}
					if len(ns) > maxXMLNamespaces {
						return root, errors.New("XML exceeds the 256-namespace limit")
					}
					if declared[prefix] || prefix == "xmlns" || a.Value == "http://www.w3.org/2000/xmlns/" ||
						(prefix == "xml" && a.Value != ns["xml"]) ||
						(prefix != "xml" && a.Value == ns["xml"]) || (prefix != "" && a.Value == "") {
						return root, errors.New("invalid or duplicate XML namespace declaration")
					}
					declared[prefix] = true
					ns[prefix] = a.Value
				}
			}
			rawName := token.Name
			token.Name, err = translate(token.Name, ns, false)
			if err != nil {
				return root, err
			}
			attrs := make(map[xml.Name]bool)
			for i, a := range token.Attr {
				if a.Name.Space == "xmlns" || (a.Name.Space == "" && a.Name.Local == "xmlns") {
					continue
				}
				token.Attr[i].Name, err = translate(a.Name, ns, true)
				if err != nil {
					return root, err
				}
				key := token.Attr[i].Name
				if attrs[key] {
					return root, errors.New("duplicate XML attribute")
				}
				attrs[key] = true
			}
			if len(stack) == 0 {
				root = token.Name
			}
			stack = append(stack, frame{raw: rawName, ns: ns})
			depth = len(stack)
			tok = token
		case xml.EndElement:
			if len(stack) == 0 || token.Name != stack[len(stack)-1].raw {
				return root, errors.New("mismatched XML closing element")
			}
			token.Name, err = translate(token.Name, stack[len(stack)-1].ns, false)
			if err != nil {
				return root, err
			}
			tok = token
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				closed = true
			}
		case xml.CharData:
			if len(stack) == 0 && (len(bytes.Trim(token, " \t\r\n")) != 0 || bytes.HasPrefix(text[textStart:], []byte("<![CDATA["))) {
				return root, errors.New("text outside XML root")
			}
		case xml.Directive:
			return root, errors.New("XML directives and DTDs are not supported")
		case xml.ProcInst:
			if strings.EqualFold(token.Target, "xml") && (token.Target != "xml" || start != offset || root.Local != "") {
				return root, errors.New("misplaced XML declaration")
			}
		}
		end := rawOffset(decoder.InputOffset())
		if visit != nil {
			if err = visit(tok, depth, start, end); err != nil {
				return root, err
			}
		}
	}
}

// UTF-16 is permitted by OPC. Transcoding is used only for validation/token
// locations: all retained output bytes still come from the original document.
func xmlText(data []byte) ([]byte, bool, int64, error) {
	if bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
		return data[3:], false, 3, nil
	}
	var order binary.ByteOrder
	if bytes.HasPrefix(data, []byte{0xff, 0xfe}) {
		order = binary.LittleEndian
	} else if bytes.HasPrefix(data, []byte{0xfe, 0xff}) {
		order = binary.BigEndian
	} else {
		return data, false, 0, nil
	}
	if len(data)%2 != 0 {
		return nil, false, 0, errors.New("odd length UTF-16 XML")
	}
	text := make([]byte, 0, len(data))
	for i := 2; i < len(data); i += 2 {
		r := rune(order.Uint16(data[i:]))
		switch {
		case r >= 0xd800 && r <= 0xdbff:
			if i+3 >= len(data) {
				return nil, false, 0, errors.New("unpaired UTF-16 surrogate")
			}
			second := rune(order.Uint16(data[i+2:]))
			if second < 0xdc00 || second > 0xdfff {
				return nil, false, 0, errors.New("unpaired UTF-16 surrogate")
			}
			r = 0x10000 + (r-0xd800)*0x400 + second - 0xdc00
			i += 2
		case r >= 0xdc00 && r <= 0xdfff:
			return nil, false, 0, errors.New("unpaired UTF-16 surrogate")
		}
		text = utf8.AppendRune(text, r)
	}
	return text, true, 2, nil
}

func protectionXML(ctx context.Context, data []byte, kind string) ([]byte, int, error) {
	protection := "sheetProtection"
	if kind == "workbook" {
		protection = "workbookProtection"
	}
	type interval struct{ start, end int64 }
	var spans []interval
	var beginning int64
	removing := false
	rootNS := ""
	root, err := scanXML(ctx, data, func(tok xml.Token, depth int, start, end int64) error {
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 1 {
				rootNS = t.Name.Space
			}
			if depth == 2 && t.Name.Space == rootNS && t.Name.Local == protection {
				beginning = start
				removing = true
			}
		case xml.EndElement:
			if removing && depth == 2 {
				spans = append(spans, interval{beginning, end})
				removing = false
			}
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if root.Local != kind || !isSpreadsheetNS(root.Space) {
		return nil, 0, fmt.Errorf("expected %s in an OOXML spreadsheet namespace", kind)
	}
	if len(spans) == 0 {
		return data, 0, nil
	}
	result := make([]byte, 0, len(data))
	var last int64
	for _, span := range spans {
		result = append(result, data[last:span.start]...)
		last = span.end
	}
	result = append(result, data[last:]...)
	return result, len(spans), nil
}
