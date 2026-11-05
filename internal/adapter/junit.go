package adapter

import (
	"bytes"
	"encoding/xml"
	"io"

	"aigate/internal/rules"
)

type junitCase struct {
	Classname string     `xml:"classname,attr"`
	Name      string     `xml:"name,attr"`
	Failure   []struct{} `xml:"failure"`
	Error     []struct{} `xml:"error"`
	Skipped   []struct{} `xml:"skipped"`
}

// ParseJUnit parses JUnit XML. data == nil means the runner produced no file
// (ParseFailed); an empty non-nil slice is an empty report. The id is "classname::name" (just name
// when classname is empty), each part normalized. error beats failure beats
// skipped; a failure whose name equals its classname and the enclosing suite
// name is an error (suite-level load failure); duplicate ids keep the worst status.
func ParseJUnit(data []byte) Parsed {
	if data == nil {
		return Parsed{Results: rules.Results{}, ParseFailed: true}
	}
	res := rules.Results{}
	var suites []string // names of the enclosing <testsuite> elements
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Parsed{Results: res, ParseFailed: true}
		}
		if ee, ok := tok.(xml.EndElement); ok && ee.Name.Local == "testsuite" && len(suites) > 0 {
			suites = suites[:len(suites)-1]
			continue
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if se.Name.Local == "testsuite" {
			name := ""
			for _, a := range se.Attr {
				if a.Name.Local == "name" {
					name = NormalizeID(a.Value)
				}
			}
			suites = append(suites, name)
			continue
		}
		if se.Name.Local != "testcase" {
			continue
		}
		var c junitCase
		if err := dec.DecodeElement(&c, &se); err != nil {
			return Parsed{Results: res, ParseFailed: true}
		}
		id := NormalizeID(c.Name)
		if cn := NormalizeID(c.Classname); cn != "" {
			id = cn + "::" + id
		}
		st := rules.Passed
		switch {
		case len(c.Error) > 0:
			st = rules.Error
		case len(c.Failure) > 0:
			st = rules.Failed
			// A failed case named like its own suite (vitest: the file path)
			// is a suite-level load failure (syntax/import error), the
			// analogue of a collection error, not a failed test.
			if len(suites) > 0 && NormalizeID(c.Name) == NormalizeID(c.Classname) && NormalizeID(c.Name) == suites[len(suites)-1] {
				st = rules.Error
			}
		case len(c.Skipped) > 0:
			st = rules.Skipped
		}
		put(res, id, st)
	}
	return Parsed{Results: res}
}
