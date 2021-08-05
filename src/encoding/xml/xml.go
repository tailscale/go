// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package xml implements a simple XML 1.0 parser that
// understands XML name spaces.
package xml

// References:
//    Annotated XML spec: https://www.xml.com/axml/testaxml.htm
//    XML name spaces: https://www.w3.org/TR/REC-xml-names/

// TODO(rsc):
//	Test error handling.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A SyntaxError represents a syntax error in the XML input stream.
type SyntaxError struct {
	Msg  string
	Line int
}

func (e *SyntaxError) Error() string {
	return "XML syntax error on line " + strconv.Itoa(e.Line) + ": " + e.Msg
}

// A Name represents an XML name (Local) annotated
// with a name space identifier (Space).
// In tokens returned by Decoder.Token, the Space identifier
// is given as a canonical URL, not the short prefix used
// in the document being parsed.
type Name struct {
	Space, Local string
}

// An Attr represents an attribute in an XML element (Name=Value).
type Attr struct {
	Name  Name
	Value string
}

// A Token is an interface holding one of the token types:
// StartElement, EndElement, CharData, Comment, ProcInst, or Directive.
type Token interface{}

// A StartElement represents an XML start element.
type StartElement struct {
	Name Name
	Attr []Attr
}

// Copy creates a new copy of StartElement.
func (e StartElement) Copy() StartElement {
	attrs := make([]Attr, len(e.Attr))
	copy(attrs, e.Attr)
	e.Attr = attrs
	return e
}

// End returns the corresponding XML end element.
func (e StartElement) End() EndElement {
	return EndElement{e.Name}
}

// An EndElement represents an XML end element.
type EndElement struct {
	Name Name
}

// A CharData represents XML character data (raw text),
// in which XML escape sequences have been replaced by
// the characters they represent.
type CharData []byte

func makeCopy(b []byte) []byte {
	b1 := make([]byte, len(b))
	copy(b1, b)
	return b1
}

// Copy creates a new copy of CharData.
func (c CharData) Copy() CharData { return CharData(makeCopy(c)) }

// A Comment represents an XML comment of the form <!--comment-->.
// The bytes do not include the <!-- and --> comment markers.
type Comment []byte

// Copy creates a new copy of Comment.
func (c Comment) Copy() Comment { return Comment(makeCopy(c)) }

// A ProcInst represents an XML processing instruction of the form <?target inst?>
type ProcInst struct {
	Target string
	Inst   []byte
}

// Copy creates a new copy of ProcInst.
func (p ProcInst) Copy() ProcInst {
	p.Inst = makeCopy(p.Inst)
	return p
}

// A Directive represents an XML directive of the form <!text>.
// The bytes do not include the <! and > markers.
type Directive []byte

// Copy creates a new copy of Directive.
func (d Directive) Copy() Directive { return Directive(makeCopy(d)) }

// CopyToken returns a copy of a Token.
func CopyToken(t Token) Token {
	switch v := t.(type) {
	case CharData:
		return v.Copy()
	case Comment:
		return v.Copy()
	case Directive:
		return v.Copy()
	case ProcInst:
		return v.Copy()
	case StartElement:
		return v.Copy()
	}
	return t
}

// A TokenReader is anything that can decode a stream of XML tokens, including a
// Decoder.
//
// When Token encounters an error or end-of-file condition after successfully
// reading a token, it returns the token. It may return the (non-nil) error from
// the same call or return the error (and a nil token) from a subsequent call.
// An instance of this general case is that a TokenReader returning a non-nil
// token at the end of the token stream may return either io.EOF or a nil error.
// The next Read should return nil, io.EOF.
//
// Implementations of Token are discouraged from returning a nil token with a
// nil error. Callers should treat a return of nil, nil as indicating that
// nothing happened; in particular it does not indicate EOF.
type TokenReader interface {
	Token() (Token, error)
}

// A Decoder represents an XML parser reading a particular input stream.
// The parser assumes that its input is encoded in UTF-8.
type Decoder struct {
	// Strict defaults to true, enforcing the requirements
	// of the XML specification.
	// If set to false, the parser allows input containing common
	// mistakes:
	//	* If an element is missing an end tag, the parser invents
	//	  end tags as necessary to keep the return values from Token
	//	  properly balanced.
	//	* In attribute values and character data, unknown or malformed
	//	  character entities (sequences beginning with &) are left alone.
	//
	// Setting:
	//
	//	d.Strict = false
	//	d.AutoClose = xml.HTMLAutoClose
	//	d.Entity = xml.HTMLEntity
	//
	// creates a parser that can handle typical HTML.
	//
	// Strict mode does not enforce the requirements of the XML name spaces TR.
	// In particular it does not reject name space tags using undefined prefixes.
	// Such tags are recorded with the unknown prefix as the name space URL.
	Strict bool

	// When Strict == false, AutoClose indicates a set of elements to
	// consider closed immediately after they are opened, regardless
	// of whether an end element is present.
	AutoClose []string

	// Entity can be used to map non-standard entity names to string replacements.
	// The parser behaves as if these standard mappings are present in the map,
	// regardless of the actual map content:
	//
	//	"lt": "<",
	//	"gt": ">",
	//	"amp": "&",
	//	"apos": "'",
	//	"quot": `"`,
	Entity map[string]string

	// CharsetReader, if non-nil, defines a function to generate
	// charset-conversion readers, converting from the provided
	// non-UTF-8 charset into UTF-8. If CharsetReader is nil or
	// returns an error, parsing stops with an error. One of the
	// CharsetReader's result values must be non-nil.
	CharsetReader func(charset string, input io.Reader) (io.Reader, error)

	// DefaultSpace sets the default name space used for unadorned tags,
	// as if the entire XML stream were wrapped in an element containing
	// the attribute xmlns="DefaultSpace".
	DefaultSpace string

	r              io.ByteReader
	t              TokenReader
	buf            bytes.Buffer
	saved          *bytes.Buffer
	stk            *stack
	free           *stack
	needClose      bool
	toClose        Name
	nextToken      Token
	nextByte       int
	ns             map[string]string
	err            error
	line           int
	offset         int64
	unmarshalDepth int
}

// NewDecoder creates a new XML parser reading from r.
// If r does not implement io.ByteReader, NewDecoder will
// do its own buffering.
func NewDecoder(r io.Reader) *Decoder {
	d := &Decoder{
		ns:       make(map[string]string),
		nextByte: -1,
		line:     1,
		Strict:   true,
	}
	d.switchToReader(r)
	return d
}

// NewTokenDecoder creates a new XML parser using an underlying token stream.
func NewTokenDecoder(t TokenReader) *Decoder {
	// Is it already a Decoder?
	if d, ok := t.(*Decoder); ok {
		return d
	}
	d := &Decoder{
		ns:       make(map[string]string),
		t:        t,
		nextByte: -1,
		line:     1,
		Strict:   true,
	}
	return d
}

// Token returns the next XML token in the input stream.
// At the end of the input stream, Token returns nil, io.EOF.
//
// Slices of bytes in the returned token data refer to the
// parser's internal buffer and remain valid only until the next
// call to Token. To acquire a copy of the bytes, call CopyToken
// or the token's Copy method.
//
// Token expands self-closing elements such as <br/>
// into separate start and end elements returned by successive calls.
//
// Token guarantees that the StartElement and EndElement
// tokens it returns are properly nested and matched:
// if Token encounters an unexpected end element
// or EOF before all expected end elements,
// it will return an error.
//
// Token implements XML name spaces as described by
// https://www.w3.org/TR/REC-xml-names/. Each of the
// Name structures contained in the Token has the Space
// set to the URL identifying its name space when known.
// If Token encounters an unrecognized name space prefix,
// it uses the prefix as the Space rather than report an error.
func (d *Decoder) Token() (Token, error) {
	var t Token
	var err error
	if d.stk != nil && d.stk.kind == stkEOF {
		return nil, io.EOF
	}
	if d.nextToken != nil {
		t = d.nextToken
		d.nextToken = nil
	} else {
		if t, err = d.rawToken(); t == nil && err != nil {
			if err == io.EOF && d.stk != nil && d.stk.kind != stkEOF {
				err = d.syntaxError("unexpected EOF")
			}
			return nil, err
		}
		// We still have a token to process, so clear any
		// errors (e.g. EOF) and proceed.
		err = nil
	}
	if !d.Strict {
		if t1, ok := d.autoClose(t); ok {
			d.nextToken = t
			t = t1
		}
	}
	switch t1 := t.(type) {
	case StartElement:
		// In XML name spaces, the translations listed in the
		// attributes apply to the element name and
		// to the other attribute names, so process
		// the translations first.
		for _, a := range t1.Attr {
			if a.Name.Space == xmlnsPrefix {
				v, ok := d.ns[a.Name.Local]
				d.pushNs(a.Name.Local, v, ok)
				d.ns[a.Name.Local] = a.Value
			}
			if a.Name.Space == "" && a.Name.Local == xmlnsPrefix {
				// Default space for untagged names
				v, ok := d.ns[""]
				d.pushNs("", v, ok)
				d.ns[""] = a.Value
			}
		}

		d.translate(&t1.Name, true)
		for i := range t1.Attr {
			d.translate(&t1.Attr[i].Name, false)
		}
		d.pushElement(t1.Name)
		t = t1

	case EndElement:
		d.translate(&t1.Name, true)
		if !d.popElement(&t1) {
			return nil, d.err
		}
		t = t1
	}
	return t, err
}

const (
	xmlURL      = "http://www.w3.org/XML/1998/namespace"
	xmlnsPrefix = "xmlns"
	xmlPrefix   = "xml"
)

// Apply name space translation to name n.
// The default name space (for Space=="")
// applies only to element names, not to attribute names.
func (d *Decoder) translate(n *Name, isElementName bool) {
	switch {
	case n.Space == xmlnsPrefix:
		return
	case n.Space == "" && !isElementName:
		return
	case n.Space == xmlPrefix:
		n.Space = xmlURL
	case n.Space == "" && n.Local == xmlnsPrefix:
		return
	}
	if v, ok := d.ns[n.Space]; ok {
		n.Space = v
	} else if n.Space == "" {
		n.Space = d.DefaultSpace
	}
}

func (d *Decoder) switchToReader(r io.Reader) {
	// Get efficient byte at a time reader.
	// Assume that if reader has its own
	// ReadByte, it's efficient enough.
	// Otherwise, use bufio.
	if rb, ok := r.(io.ByteReader); ok {
		d.r = rb
	} else {
		d.r = bufio.NewReader(r)
	}
}

// Parsing state - stack holds old name space translations
// and the current set of open elements. The translations to pop when
// ending a given tag are *below* it on the stack, which is
// more work but forced on us by XML.
type stack struct {
	next *stack
	kind int
	name Name
	ok   bool
}

const (
	stkStart = iota
	stkNs
	stkEOF
)

func (d *Decoder) push(kind int) *stack {
	s := d.free
	if s != nil {
		d.free = s.next
	} else {
		s = new(stack)
	}
	s.next = d.stk
	s.kind = kind
	d.stk = s
	return s
}

func (d *Decoder) pop() *stack {
	s := d.stk
	if s != nil {
		d.stk = s.next
		s.next = d.free
		d.free = s
	}
	return s
}

// Record that after the current element is finished
// (that element is already pushed on the stack)
// Token should return EOF until popEOF is called.
func (d *Decoder) pushEOF() {
	// Walk down stack to find Start.
	// It might not be the top, because there might be stkNs
	// entries above it.
	start := d.stk
	for start.kind != stkStart {
		start = start.next
	}
	// The stkNs entries below a start are associated with that
	// element too; skip over them.
	for start.next != nil && start.next.kind == stkNs {
		start = start.next
	}
	s := d.free
	if s != nil {
		d.free = s.next
	} else {
		s = new(stack)
	}
	s.kind = stkEOF
	s.next = start.next
	start.next = s
}

// Undo a pushEOF.
// The element must have been finished, so the EOF should be at the top of the stack.
func (d *Decoder) popEOF() bool {
	if d.stk == nil || d.stk.kind != stkEOF {
		return false
	}
	d.pop()
	return true
}

// Record that we are starting an element with the given name.
func (d *Decoder) pushElement(name Name) {
	s := d.push(stkStart)
	s.name = name
}

// Record that we are changing the value of ns[local].
// The old value is url, ok.
func (d *Decoder) pushNs(local string, url string, ok bool) {
	s := d.push(stkNs)
	s.name.Local = local
	s.name.Space = url
	s.ok = ok
}

// Creates a SyntaxError with the current line number.
func (d *Decoder) syntaxError(msg string) error {
	return &SyntaxError{Msg: msg, Line: d.line}
}

// Record that we are ending an element with the given name.
// The name must match the record at the top of the stack,
// which must be a pushElement record.
// After popping the element, apply any undo records from
// the stack to restore the name translations that existed
// before we saw this element.
func (d *Decoder) popElement(t *EndElement) bool {
	s := d.pop()
	name := t.Name
	switch {
	case s == nil || s.kind != stkStart:
		d.err = d.syntaxError("unexpected end element </" + name.Local + ">")
		return false
	case s.name.Local != name.Local:
		if !d.Strict {
			d.needClose = true
			d.toClose = t.Name
			t.Name = s.name
			return true
		}
		d.err = d.syntaxError("element <" + s.name.Local + "> closed by </" + name.Local + ">")
		return false
	case s.name.Space != name.Space:
		d.err = d.syntaxError("element <" + s.name.Local + "> in space " + s.name.Space +
			"closed by </" + name.Local + "> in space " + name.Space)
		return false
	}

	// Pop stack until a Start or EOF is on the top, undoing the
	// translations that were associated with the element we just closed.
	for d.stk != nil && d.stk.kind != stkStart && d.stk.kind != stkEOF {
		s := d.pop()
		if s.ok {
			d.ns[s.name.Local] = s.name.Space
		} else {
			delete(d.ns, s.name.Local)
		}
	}

	return true
}

// If the top element on the stack is autoclosing and
// t is not the end tag, invent the end tag.
func (d *Decoder) autoClose(t Token) (Token, bool) {
	if d.stk == nil || d.stk.kind != stkStart {
		return nil, false
	}
	name := strings.ToLower(d.stk.name.Local)
	for _, s := range d.AutoClose {
		if strings.ToLower(s) == name {
			// This one should be auto closed if t doesn't close it.
			et, ok := t.(EndElement)
			if !ok || et.Name.Local != name {
				return EndElement{d.stk.name}, true
			}
			break
		}
	}
	return nil, false
}

var errRawToken = errors.New("xml: cannot use RawToken from UnmarshalXML method")

// RawToken is like Token but does not verify that
// start and end elements match and does not translate
// name space prefixes to their corresponding URLs.
func (d *Decoder) RawToken() (Token, error) {
	if d.unmarshalDepth > 0 {
		return nil, errRawToken
	}
	return d.rawToken()
}

func (d *Decoder) rawToken() (Token, error) {
	if d.t != nil {
		return d.t.Token()
	}
	if d.err != nil {
		return nil, d.err
	}
	if d.needClose {
		// The last element we read was self-closing and
		// we returned just the StartElement half.
		// Return the EndElement half now.
		d.needClose = false
		return EndElement{d.toClose}, nil
	}

	b, ok := d.getc()
	if !ok {
		return nil, d.err
	}

	if b != '<' {
		// Text section.
		d.ungetc(b)
		data := d.text(-1, false)
		if data == nil {
			return nil, d.err
		}
		return CharData(data), nil
	}

	if b, ok = d.mustgetc(); !ok {
		return nil, d.err
	}
	switch b {
	case '/':
		// </: End element
		var name Name
		if name, ok = d.nsname(); !ok {
			if d.err == nil {
				d.err = d.syntaxError("expected element name after </")
			}
			return nil, d.err
		}
		d.space()
		if b, ok = d.mustgetc(); !ok {
			return nil, d.err
		}
		if b != '>' {
			d.err = d.syntaxError("invalid characters between </" + name.Local + " and >")
			return nil, d.err
		}
		return EndElement{name}, nil

	case '?':
		// <?: Processing instruction.
		var target string
		if target, ok = d.name(); !ok {
			if d.err == nil {
				d.err = d.syntaxError("expected target name after <?")
			}
			return nil, d.err
		}
		d.space()
		d.buf.Reset()
		var b0 byte
		for {
			if b, ok = d.mustgetc(); !ok {
				return nil, d.err
			}
			d.buf.WriteByte(b)
			if b0 == '?' && b == '>' {
				break
			}
			b0 = b
		}
		data := d.buf.Bytes()
		data = data[0 : len(data)-2] // chop ?>

		if target == "xml" {
			content := string(data)
			ver := procInst("version", content)
			if ver != "" && ver != "1.0" {
				d.err = fmt.Errorf("xml: unsupported version %q; only version 1.0 is supported", ver)
				return nil, d.err
			}
			enc := procInst("encoding", content)
			if enc != "" && enc != "utf-8" && enc != "UTF-8" && !strings.EqualFold(enc, "utf-8") {
				if d.CharsetReader == nil {
					d.err = fmt.Errorf("xml: encoding %q declared but Decoder.CharsetReader is nil", enc)
					return nil, d.err
				}
				newr, err := d.CharsetReader(enc, d.r.(io.Reader))
				if err != nil {
					d.err = fmt.Errorf("xml: opening charset %q: %v", enc, err)
					return nil, d.err
				}
				if newr == nil {
					panic("CharsetReader returned a nil Reader for charset " + enc)
				}
				d.switchToReader(newr)
			}
		}
		return ProcInst{target, data}, nil

	case '!':
		// <!: Maybe comment, maybe CDATA.
		if b, ok = d.mustgetc(); !ok {
			return nil, d.err
		}
		switch b {
		case '-': // <!-
			// Probably <!-- for a comment.
			if b, ok = d.mustgetc(); !ok {
				return nil, d.err
			}
			if b != '-' {
				d.err = d.syntaxError("invalid sequence <!- not part of <!--")
				return nil, d.err
			}
			// Look for terminator.
			d.buf.Reset()
			var b0, b1 byte
			for {
				if b, ok = d.mustgetc(); !ok {
					return nil, d.err
				}
				d.buf.WriteByte(b)
				if b0 == '-' && b1 == '-' {
					if b != '>' {
						d.err = d.syntaxError(
							`invalid sequence "--" not allowed in comments`)
						return nil, d.err
					}
					break
				}
				b0, b1 = b1, b
			}
			data := d.buf.Bytes()
			data = data[0 : len(data)-3] // chop -->
			return Comment(data), nil

		case '[': // <![
			// Probably <![CDATA[.
			for i := 0; i < 6; i++ {
				if b, ok = d.mustgetc(); !ok {
					return nil, d.err
				}
				if b != "CDATA["[i] {
					d.err = d.syntaxError("invalid <![ sequence")
					return nil, d.err
				}
			}
			// Have <![CDATA[.  Read text until ]]>.
			data := d.text(-1, true)
			if data == nil {
				return nil, d.err
			}
			return CharData(data), nil
		}

		// Probably a directive: <!DOCTYPE ...>, <!ENTITY ...>, etc.
		// We don't care, but accumulate for caller. Quoted angle
		// brackets do not count for nesting.
		d.buf.Reset()
		d.buf.WriteByte(b)
		inquote := uint8(0)
		depth := 0
		for {
			if b, ok = d.mustgetc(); !ok {
				return nil, d.err
			}
			if inquote == 0 && b == '>' && depth == 0 {
				break
			}
		HandleB:
			d.buf.WriteByte(b)
			switch {
			case b == inquote:
				inquote = 0

			case inquote != 0:
				// in quotes, no special action

			case b == '\'' || b == '"':
				inquote = b

			case b == '>' && inquote == 0:
				depth--

			case b == '<' && inquote == 0:
				// Look for <!-- to begin comment.
				s := "!--"
				for i := 0; i < len(s); i++ {
					if b, ok = d.mustgetc(); !ok {
						return nil, d.err
					}
					if b != s[i] {
						for j := 0; j < i; j++ {
							d.buf.WriteByte(s[j])
						}
						depth++
						goto HandleB
					}
				}

				// Remove < that was written above.
				d.buf.Truncate(d.buf.Len() - 1)

				// Look for terminator.
				var b0, b1 byte
				for {
					if b, ok = d.mustgetc(); !ok {
						return nil, d.err
					}
					if b0 == '-' && b1 == '-' && b == '>' {
						break
					}
					b0, b1 = b1, b
				}
			}
		}
		return Directive(d.buf.Bytes()), nil
	}

	// Must be an open element like <a href="foo">
	d.ungetc(b)

	var (
		name  Name
		empty bool
		attr  []Attr
	)
	if name, ok = d.nsname(); !ok {
		if d.err == nil {
			d.err = d.syntaxError("expected element name after <")
		}
		return nil, d.err
	}

	attr = []Attr{}
	for {
		d.space()
		if b, ok = d.mustgetc(); !ok {
			return nil, d.err
		}
		if b == '/' {
			empty = true
			if b, ok = d.mustgetc(); !ok {
				return nil, d.err
			}
			if b != '>' {
				d.err = d.syntaxError("expected /> in element")
				return nil, d.err
			}
			break
		}
		if b == '>' {
			break
		}
		d.ungetc(b)

		a := Attr{}
		if a.Name, ok = d.nsname(); !ok {
			if d.err == nil {
				d.err = d.syntaxError("expected attribute name in element")
			}
			return nil, d.err
		}
		d.space()
		if b, ok = d.mustgetc(); !ok {
			return nil, d.err
		}
		if b != '=' {
			if d.Strict {
				d.err = d.syntaxError("attribute name without = in element")
				return nil, d.err
			}
			d.ungetc(b)
			a.Value = a.Name.Local
		} else {
			d.space()
			data := d.attrval()
			if data == nil {
				return nil, d.err
			}
			a.Value = string(data)
		}
		attr = append(attr, a)
	}
	if empty {
		d.needClose = true
		d.toClose = name
	}
	return StartElement{name, attr}, nil
}

func (d *Decoder) attrval() []byte {
	b, ok := d.mustgetc()
	if !ok {
		return nil
	}
	// Handle quoted attribute values
	if b == '"' || b == '\'' {
		return d.text(int(b), false)
	}
	// Handle unquoted attribute values for strict parsers
	if d.Strict {
		d.err = d.syntaxError("unquoted or missing attribute value in element")
		return nil
	}
	// Handle unquoted attribute values for unstrict parsers
	d.ungetc(b)
	d.buf.Reset()
	for {
		b, ok = d.mustgetc()
		if !ok {
			return nil
		}
		// https://www.w3.org/TR/REC-html40/intro/sgmltut.html#h-3.2.2
		if 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' ||
			'0' <= b && b <= '9' || b == '_' || b == ':' || b == '-' {
			d.buf.WriteByte(b)
		} else {
			d.ungetc(b)
			break
		}
	}
	return d.buf.Bytes()
}

// Skip spaces if any
func (d *Decoder) space() {
	for {
		b, ok := d.getc()
		if !ok {
			return
		}
		switch b {
		case ' ', '\r', '\n', '\t':
		default:
			d.ungetc(b)
			return
		}
	}
}

// Read a single byte.
// If there is no byte to read, return ok==false
// and leave the error in d.err.
// Maintain line number.
func (d *Decoder) getc() (b byte, ok bool) {
	if d.err != nil {
		return 0, false
	}
	if d.nextByte >= 0 {
		b = byte(d.nextByte)
		d.nextByte = -1
	} else {
		b, d.err = d.r.ReadByte()
		if d.err != nil {
			return 0, false
		}
		if d.saved != nil {
			d.saved.WriteByte(b)
		}
	}
	if b == '\n' {
		d.line++
	}
	d.offset++
	return b, true
}

// InputOffset returns the input stream byte offset of the current decoder position.
// The offset gives the location of the end of the most recently returned token
// and the beginning of the next token.
func (d *Decoder) InputOffset() int64 {
	return d.offset
}

// Return saved offset.
// If we did ungetc (nextByte >= 0), have to back up one.
func (d *Decoder) savedOffset() int {
	n := d.saved.Len()
	if d.nextByte >= 0 {
		n--
	}
	return n
}

// Must read a single byte.
// If there is no byte to read,
// set d.err to SyntaxError("unexpected EOF")
// and return ok==false
func (d *Decoder) mustgetc() (b byte, ok bool) {
	if b, ok = d.getc(); !ok {
		if d.err == io.EOF {
			d.err = d.syntaxError("unexpected EOF")
		}
	}
	return
}

// Unread a single byte.
func (d *Decoder) ungetc(b byte) {
	if b == '\n' {
		d.line--
	}
	d.nextByte = int(b)
	d.offset--
}

var entity = map[string]rune{
	"lt":   '<',
	"gt":   '>',
	"amp":  '&',
	"apos": '\'',
	"quot": '"',
}

// Read plain text section (XML calls it character data).
// If quote >= 0, we are in a quoted string and need to find the matching quote.
// If cdata == true, we are in a <![CDATA[ section and need to find ]]>.
// On failure return nil and leave the error in d.err.
func (d *Decoder) text(quote int, cdata bool) []byte {
	var b0, b1 byte
	var trunc int
	d.buf.Reset()
Input:
	for {
		b, ok := d.getc()
		if !ok {
			if cdata {
				if d.err == io.EOF {
					d.err = d.syntaxError("unexpected EOF in CDATA section")
				}
				return nil
			}
			break Input
		}

		// <![CDATA[ section ends with ]]>.
		// It is an error for ]]> to appear in ordinary text.
		if b0 == ']' && b1 == ']' && b == '>' {
			if cdata {
				trunc = 2
				break Input
			}
			d.err = d.syntaxError("unescaped ]]> not in CDATA section")
			return nil
		}

		// Stop reading text if we see a <.
		if b == '<' && !cdata {
			if quote >= 0 {
				d.err = d.syntaxError("unescaped < inside quoted string")
				return nil
			}
			d.ungetc('<')
			break Input
		}
		if quote >= 0 && b == byte(quote) {
			break Input
		}
		if b == '&' && !cdata {
			// Read escaped character expression up to semicolon.
			// XML in all its glory allows a document to define and use
			// its own character names with <!ENTITY ...> directives.
			// Parsers are required to recognize lt, gt, amp, apos, and quot
			// even if they have not been declared.
			before := d.buf.Len()
			d.buf.WriteByte('&')
			var ok bool
			var text string
			var haveText bool
			if b, ok = d.mustgetc(); !ok {
				return nil
			}
			if b == '#' {
				d.buf.WriteByte(b)
				if b, ok = d.mustgetc(); !ok {
					return nil
				}
				base := 10
				if b == 'x' {
					base = 16
					d.buf.WriteByte(b)
					if b, ok = d.mustgetc(); !ok {
						return nil
					}
				}
				start := d.buf.Len()
				for '0' <= b && b <= '9' ||
					base == 16 && 'a' <= b && b <= 'f' ||
					base == 16 && 'A' <= b && b <= 'F' {
					d.buf.WriteByte(b)
					if b, ok = d.mustgetc(); !ok {
						return nil
					}
				}
				if b != ';' {
					d.ungetc(b)
				} else {
					s := string(d.buf.Bytes()[start:])
					d.buf.WriteByte(';')
					n, err := strconv.ParseUint(s, base, 64)
					if err == nil && n <= unicode.MaxRune {
						text = string(rune(n))
						haveText = true
					}
				}
			} else {
				d.ungetc(b)
				if !d.readName() {
					if d.err != nil {
						return nil
					}
				}
				if b, ok = d.mustgetc(); !ok {
					return nil
				}
				if b != ';' {
					d.ungetc(b)
				} else {
					name := d.buf.Bytes()[before+1:]
					d.buf.WriteByte(';')
					if isName(name) {
						s := string(name)
						if r, ok := entity[s]; ok {
							text = string(r)
							haveText = true
						} else if d.Entity != nil {
							text, haveText = d.Entity[s]
						}
					}
				}
			}

			if haveText {
				d.buf.Truncate(before)
				d.buf.Write([]byte(text))
				b0, b1 = 0, 0
				continue Input
			}
			if !d.Strict {
				b0, b1 = 0, 0
				continue Input
			}
			ent := string(d.buf.Bytes()[before:])
			if ent[len(ent)-1] != ';' {
				ent += " (no semicolon)"
			}
			d.err = d.syntaxError("invalid character entity " + ent)
			return nil
		}

		// We must rewrite unescaped \r and \r\n into \n.
		if b == '\r' {
			d.buf.WriteByte('\n')
		} else if b1 == '\r' && b == '\n' {
			// Skip \r\n--we already wrote \n.
		} else {
			d.buf.WriteByte(b)
		}

		b0, b1 = b1, b
	}
	data := d.buf.Bytes()
	data = data[0 : len(data)-trunc]

	// Inspect each rune for being a disallowed character.
	buf := data
	for len(buf) > 0 {
		r, size := utf8.DecodeRune(buf)
		if r == utf8.RuneError && size == 1 {
			d.err = d.syntaxError("invalid UTF-8")
			return nil
		}
		buf = buf[size:]
		if !isInCharacterRange(r) {
			d.err = d.syntaxError(fmt.Sprintf("illegal character code %U", r))
			return nil
		}
	}

	return data
}

// Decide whether the given rune is in the XML Character Range, per
// the Char production of https://www.xml.com/axml/testaxml.htm,
// Section 2.2 Characters.
func isInCharacterRange(r rune) (inrange bool) {
	return r == 0x09 ||
		r == 0x0A ||
		r == 0x0D ||
		r >= 0x20 && r <= 0xD7FF ||
		r >= 0xE000 && r <= 0xFFFD ||
		r >= 0x10000 && r <= 0x10FFFF
}

// Get name space name: name with a : stuck in the middle.
// The part before the : is the name space identifier.
func (d *Decoder) nsname() (name Name, ok bool) {
	s, ok := d.name()
	if !ok {
		return
	}
	i := strings.Index(s, ":")
	if i < 0 {
		name.Local = s
	} else {
		name.Space = s[0:i]
		name.Local = s[i+1:]
	}
	return name, true
}

// Get name: /first(first|second)*/
// Do not set d.err if the name is missing (unless unexpected EOF is received):
// let the caller provide better context.
func (d *Decoder) name() (s string, ok bool) {
	d.buf.Reset()
	if !d.readName() {
		return "", false
	}

	// Now we check the characters.
	b := d.buf.Bytes()
	if !isName(b) {
		d.err = d.syntaxError("invalid XML name: " + string(b))
		return "", false
	}
	return string(b), true
}

// Read a name and append its bytes to d.buf.
// The name is delimited by any single-byte character not valid in names.
// All multi-byte characters are accepted; the caller must check their validity.
func (d *Decoder) readName() (ok bool) {
	var b byte
	if b, ok = d.mustgetc(); !ok {
		return
	}
	if b < utf8.RuneSelf && !isNameByte(b) {
		d.ungetc(b)
		return false
	}
	d.buf.WriteByte(b)

	for {
		if b, ok = d.mustgetc(); !ok {
			return
		}
		if b < utf8.RuneSelf && !isNameByte(b) {
			d.ungetc(b)
			break
		}
		d.buf.WriteByte(b)
	}
	return true
}

func isNameByte(c byte) bool {
	return 'A' <= c && c <= 'Z' ||
		'a' <= c && c <= 'z' ||
		'0' <= c && c <= '9' ||
		c == '_' || c == ':' || c == '.' || c == '-'
}

func isName(s []byte) bool {
	if len(s) == 0 {
		return false
	}
	c, n := utf8.DecodeRune(s)
	if c == utf8.RuneError && n == 1 {
		return false
	}
	if !unicode.Is(first, c) {
		return false
	}
	for n < len(s) {
		s = s[n:]
		c, n = utf8.DecodeRune(s)
		if c == utf8.RuneError && n == 1 {
			return false
		}
		if !unicode.Is(first, c) && !unicode.Is(second, c) {
			return false
		}
	}
	return true
}

func isNameString(s string) bool {
	if len(s) == 0 {
		return false
	}
	c, n := utf8.DecodeRuneInString(s)
	if c == utf8.RuneError && n == 1 {
		return false
	}
	if !unicode.Is(first, c) {
		return false
	}
	for n < len(s) {
		s = s[n:]
		c, n = utf8.DecodeRuneInString(s)
		if c == utf8.RuneError && n == 1 {
			return false
		}
		if !unicode.Is(first, c) && !unicode.Is(second, c) {
			return false
		}
	}
	return true
}

// These tables were generated by cut and paste from Appendix B of
// the XML spec at https://www.xml.com/axml/testaxml.htm
// and then reformatting. First corresponds to (Letter | '_' | ':')
// and second corresponds to NameChar.
const first = unicode.XMLFirst
const second = unicode.XMLSecond

// HTMLEntity is an entity map containing translations for the
// standard HTML entity characters.
//
// See the Decoder.Strict and Decoder.Entity fields' documentation.
var HTMLEntity map[string]string = htmlEntity

var htmlEntity = map[string]string{
	/*
		hget http://www.w3.org/TR/html4/sgml/entities.html |
		ssam '
			,y /\&gt;/ x/\&lt;(.|\n)+/ s/\n/ /g
			,x v/^\&lt;!ENTITY/d
			,s/\&lt;!ENTITY ([^ ]+) .*U\+([0-9A-F][0-9A-F][0-9A-F][0-9A-F]) .+/	"\1": "\\u\2",/g
		'
	*/
	"nbsp":     "\u00A0",
	"iexcl":    "\u00A1",
	"cent":     "\u00A2",
	"pound":    "\u00A3",
	"curren":   "\u00A4",
	"yen":      "\u00A5",
	"brvbar":   "\u00A6",
	"sect":     "\u00A7",
	"uml":      "\u00A8",
	"copy":     "\u00A9",
	"ordf":     "\u00AA",
	"laquo":    "\u00AB",
	"not":      "\u00AC",
	"shy":      "\u00AD",
	"reg":      "\u00AE",
	"macr":     "\u00AF",
	"deg":      "\u00B0",
	"plusmn":   "\u00B1",
	"sup2":     "\u00B2",
	"sup3":     "\u00B3",
	"acute":    "\u00B4",
	"micro":    "\u00B5",
	"para":     "\u00B6",
	"middot":   "\u00B7",
	"cedil":    "\u00B8",
	"sup1":     "\u00B9",
	"ordm":     "\u00BA",
	"raquo":    "\u00BB",
	"frac14":   "\u00BC",
	"frac12":   "\u00BD",
	"frac34":   "\u00BE",
	"iquest":   "\u00BF",
	"Agrave":   "\u00C0",
	"Aacute":   "\u00C1",
	"Acirc":    "\u00C2",
	"Atilde":   "\u00C3",
	"Auml":     "\u00C4",
	"Aring":    "\u00C5",
	"AElig":    "\u00C6",
	"Ccedil":   "\u00C7",
	"Egrave":   "\u00C8",
	"Eacute":   "\u00C9",
	"Ecirc":    "\u00CA",
	"Euml":     "\u00CB",
	"Igrave":   "\u00CC",
	"Iacute":   "\u00CD",
	"Icirc":    "\u00CE",
	"Iuml":     "\u00CF",
	"ETH":      "\u00D0",
	"Ntilde":   "\u00D1",
	"Ograve":   "\u00D2",
	"Oacute":   "\u00D3",
	"Ocirc":    "\u00D4",
	"Otilde":   "\u00D5",
	"Ouml":     "\u00D6",
	"times":    "\u00D7",
	"Oslash":   "\u00D8",
	"Ugrave":   "\u00D9",
	"Uacute":   "\u00DA",
	"Ucirc":    "\u00DB",
	"Uuml":     "\u00DC",
	"Yacute":   "\u00DD",
	"THORN":    "\u00DE",
	"szlig":    "\u00DF",
	"agrave":   "\u00E0",
	"aacute":   "\u00E1",
	"acirc":    "\u00E2",
	"atilde":   "\u00E3",
	"auml":     "\u00E4",
	"aring":    "\u00E5",
	"aelig":    "\u00E6",
	"ccedil":   "\u00E7",
	"egrave":   "\u00E8",
	"eacute":   "\u00E9",
	"ecirc":    "\u00EA",
	"euml":     "\u00EB",
	"igrave":   "\u00EC",
	"iacute":   "\u00ED",
	"icirc":    "\u00EE",
	"iuml":     "\u00EF",
	"eth":      "\u00F0",
	"ntilde":   "\u00F1",
	"ograve":   "\u00F2",
	"oacute":   "\u00F3",
	"ocirc":    "\u00F4",
	"otilde":   "\u00F5",
	"ouml":     "\u00F6",
	"divide":   "\u00F7",
	"oslash":   "\u00F8",
	"ugrave":   "\u00F9",
	"uacute":   "\u00FA",
	"ucirc":    "\u00FB",
	"uuml":     "\u00FC",
	"yacute":   "\u00FD",
	"thorn":    "\u00FE",
	"yuml":     "\u00FF",
	"fnof":     "\u0192",
	"Alpha":    "\u0391",
	"Beta":     "\u0392",
	"Gamma":    "\u0393",
	"Delta":    "\u0394",
	"Epsilon":  "\u0395",
	"Zeta":     "\u0396",
	"Eta":      "\u0397",
	"Theta":    "\u0398",
	"Iota":     "\u0399",
	"Kappa":    "\u039A",
	"Lambda":   "\u039B",
	"Mu":       "\u039C",
	"Nu":       "\u039D",
	"Xi":       "\u039E",
	"Omicron":  "\u039F",
	"Pi":       "\u03A0",
	"Rho":      "\u03A1",
	"Sigma":    "\u03A3",
	"Tau":      "\u03A4",
	"Upsilon":  "\u03A5",
	"Phi":      "\u03A6",
	"Chi":      "\u03A7",
	"Psi":      "\u03A8",
	"Omega":    "\u03A9",
	"alpha":    "\u03B1",
	"beta":     "\u03B2",
	"gamma":    "\u03B3",
	"delta":    "\u03B4",
	"epsilon":  "\u03B5",
	"zeta":     "\u03B6",
	"eta":      "\u03B7",
	"theta":    "\u03B8",
	"iota":     "\u03B9",
	"kappa":    "\u03BA",
	"lambda":   "\u03BB",
	"mu":       "\u03BC",
	"nu":       "\u03BD",
	"xi":       "\u03BE",
	"omicron":  "\u03BF",
	"pi":       "\u03C0",
	"rho":      "\u03C1",
	"sigmaf":   "\u03C2",
	"sigma":    "\u03C3",
	"tau":      "\u03C4",
	"upsilon":  "\u03C5",
	"phi":      "\u03C6",
	"chi":      "\u03C7",
	"psi":      "\u03C8",
	"omega":    "\u03C9",
	"thetasym": "\u03D1",
	"upsih":    "\u03D2",
	"piv":      "\u03D6",
	"bull":     "\u2022",
	"hellip":   "\u2026",
	"prime":    "\u2032",
	"Prime":    "\u2033",
	"oline":    "\u203E",
	"frasl":    "\u2044",
	"weierp":   "\u2118",
	"image":    "\u2111",
	"real":     "\u211C",
	"trade":    "\u2122",
	"alefsym":  "\u2135",
	"larr":     "\u2190",
	"uarr":     "\u2191",
	"rarr":     "\u2192",
	"darr":     "\u2193",
	"harr":     "\u2194",
	"crarr":    "\u21B5",
	"lArr":     "\u21D0",
	"uArr":     "\u21D1",
	"rArr":     "\u21D2",
	"dArr":     "\u21D3",
	"hArr":     "\u21D4",
	"forall":   "\u2200",
	"part":     "\u2202",
	"exist":    "\u2203",
	"empty":    "\u2205",
	"nabla":    "\u2207",
	"isin":     "\u2208",
	"notin":    "\u2209",
	"ni":       "\u220B",
	"prod":     "\u220F",
	"sum":      "\u2211",
	"minus":    "\u2212",
	"lowast":   "\u2217",
	"radic":    "\u221A",
	"prop":     "\u221D",
	"infin":    "\u221E",
	"ang":      "\u2220",
	"and":      "\u2227",
	"or":       "\u2228",
	"cap":      "\u2229",
	"cup":      "\u222A",
	"int":      "\u222B",
	"there4":   "\u2234",
	"sim":      "\u223C",
	"cong":     "\u2245",
	"asymp":    "\u2248",
	"ne":       "\u2260",
	"equiv":    "\u2261",
	"le":       "\u2264",
	"ge":       "\u2265",
	"sub":      "\u2282",
	"sup":      "\u2283",
	"nsub":     "\u2284",
	"sube":     "\u2286",
	"supe":     "\u2287",
	"oplus":    "\u2295",
	"otimes":   "\u2297",
	"perp":     "\u22A5",
	"sdot":     "\u22C5",
	"lceil":    "\u2308",
	"rceil":    "\u2309",
	"lfloor":   "\u230A",
	"rfloor":   "\u230B",
	"lang":     "\u2329",
	"rang":     "\u232A",
	"loz":      "\u25CA",
	"spades":   "\u2660",
	"clubs":    "\u2663",
	"hearts":   "\u2665",
	"diams":    "\u2666",
	"quot":     "\u0022",
	"amp":      "\u0026",
	"lt":       "\u003C",
	"gt":       "\u003E",
	"OElig":    "\u0152",
	"oelig":    "\u0153",
	"Scaron":   "\u0160",
	"scaron":   "\u0161",
	"Yuml":     "\u0178",
	"circ":     "\u02C6",
	"tilde":    "\u02DC",
	"ensp":     "\u2002",
	"emsp":     "\u2003",
	"thinsp":   "\u2009",
	"zwnj":     "\u200C",
	"zwj":      "\u200D",
	"lrm":      "\u200E",
	"rlm":      "\u200F",
	"ndash":    "\u2013",
	"mdash":    "\u2014",
	"lsquo":    "\u2018",
	"rsquo":    "\u2019",
	"sbquo":    "\u201A",
	"ldquo":    "\u201C",
	"rdquo":    "\u201D",
	"bdquo":    "\u201E",
	"dagger":   "\u2020",
	"Dagger":   "\u2021",
	"permil":   "\u2030",
	"lsaquo":   "\u2039",
	"rsaquo":   "\u203A",
	"euro":     "\u20AC",
}

// HTMLAutoClose is the set of HTML elements that
// should be considered to close automatically.
//
// See the Decoder.Strict and Decoder.Entity fields' documentation.
var HTMLAutoClose []string = htmlAutoClose

var htmlAutoClose = []string{
	/*
		hget http://www.w3.org/TR/html4/loose.dtd |
		9 sed -n 's/<!ELEMENT ([^ ]*) +- O EMPTY.+/	"\1",/p' | tr A-Z a-z
	*/
	"basefont",
	"br",
	"area",
	"link",
	"img",
	"param",
	"hr",
	"input",
	"col",
	"frame",
	"isindex",
	"base",
	"meta",
}

var (
	escQuot = []byte("&#34;") // shorter than "&quot;"
	escApos = []byte("&#39;") // shorter than "&apos;"
	escAmp  = []byte("&amp;")
	escLT   = []byte("&lt;")
	escGT   = []byte("&gt;")
	escTab  = []byte("&#x9;")
	escNL   = []byte("&#xA;")
	escCR   = []byte("&#xD;")
	escFFFD = []byte("\uFFFD") // Unicode replacement character
)

// EscapeText writes to w the properly escaped XML equivalent
// of the plain text data s.
func EscapeText(w io.Writer, s []byte) error {
	return escapeText(w, s, true)
}

// escapeText writes to w the properly escaped XML equivalent
// of the plain text data s. If escapeNewline is true, newline
// characters will be escaped.
func escapeText(w io.Writer, s []byte, escapeNewline bool) error {
	var esc []byte
	last := 0
	for i := 0; i < len(s); {
		r, width := utf8.DecodeRune(s[i:])
		i += width
		switch r {
		case '"':
			esc = escQuot
		case '\'':
			esc = escApos
		case '&':
			esc = escAmp
		case '<':
			esc = escLT
		case '>':
			esc = escGT
		case '\t':
			esc = escTab
		case '\n':
			if !escapeNewline {
				continue
			}
			esc = escNL
		case '\r':
			esc = escCR
		default:
			if !isInCharacterRange(r) || (r == 0xFFFD && width == 1) {
				esc = escFFFD
				break
			}
			continue
		}
		if _, err := w.Write(s[last : i-width]); err != nil {
			return err
		}
		if _, err := w.Write(esc); err != nil {
			return err
		}
		last = i
	}
	_, err := w.Write(s[last:])
	return err
}

// EscapeString writes to p the properly escaped XML equivalent
// of the plain text data s.
func (p *printer) EscapeString(s string) {
	var esc []byte
	last := 0
	for i := 0; i < len(s); {
		r, width := utf8.DecodeRuneInString(s[i:])
		i += width
		switch r {
		case '"':
			esc = escQuot
		case '\'':
			esc = escApos
		case '&':
			esc = escAmp
		case '<':
			esc = escLT
		case '>':
			esc = escGT
		case '\t':
			esc = escTab
		case '\n':
			esc = escNL
		case '\r':
			esc = escCR
		default:
			if !isInCharacterRange(r) || (r == 0xFFFD && width == 1) {
				esc = escFFFD
				break
			}
			continue
		}
		p.WriteString(s[last : i-width])
		p.Write(esc)
		last = i
	}
	p.WriteString(s[last:])
}

// Escape is like EscapeText but omits the error return value.
// It is provided for backwards compatibility with Go 1.0.
// Code targeting Go 1.1 or later should use EscapeText.
func Escape(w io.Writer, s []byte) {
	EscapeText(w, s)
}

var (
	cdataStart  = []byte("<![CDATA[")
	cdataEnd    = []byte("]]>")
	cdataEscape = []byte("]]]]><![CDATA[>")
)

// emitCDATA writes to w the CDATA-wrapped plain text data s.
// It escapes CDATA directives nested in s.
func emitCDATA(w io.Writer, s []byte) error {
	if len(s) == 0 {
		return nil
	}
	if _, err := w.Write(cdataStart); err != nil {
		return err
	}
	for {
		i := bytes.Index(s, cdataEnd)
		if i >= 0 && i+len(cdataEnd) <= len(s) {
			// Found a nested CDATA directive end.
			if _, err := w.Write(s[:i]); err != nil {
				return err
			}
			if _, err := w.Write(cdataEscape); err != nil {
				return err
			}
			i += len(cdataEnd)
		} else {
			if _, err := w.Write(s); err != nil {
				return err
			}
			break
		}
		s = s[i:]
	}
	_, err := w.Write(cdataEnd)
	return err
}

// procInst parses the `param="..."` or `param='...'`
// value out of the provided string, returning "" if not found.
func procInst(param, s string) string {
	// TODO: this parsing is somewhat lame and not exact.
	// It works for all actual cases, though.
	param = param + "="
	idx := strings.Index(s, param)
	if idx == -1 {
		return ""
	}
	v := s[idx+len(param):]
	if v == "" {
		return ""
	}
	if v[0] != '\'' && v[0] != '"' {
		return ""
	}
	idx = strings.IndexRune(v[1:], rune(v[0]))
	if idx == -1 {
		return ""
	}
	return v[1 : idx+1]
}
