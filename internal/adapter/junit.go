package adapter

import (
	"encoding/xml"
	"io"
	"os"

	"aigate/internal/rules"
)

type junitCase struct {
	Classname string     `xml:"classname,attr"`
	Name      string     `xml:"name,attr"`
	Failure   []struct{} `xml:"failure"`
	Error     []struct{} `xml:"error"`
	Skipped   []struct{} `xml:"skipped"`
}

// ParseJUnit reads a JUnit XML file. The id is "classname::name" (just name
// when classname is empty), each part normalized. error beats failure beats
// skipped; duplicate ids keep the worst status.
func ParseJUnit(path string) Parsed {
	f, err := os.Open(path)
	if err != nil {
		return Parsed{Results: rules.Results{}, ParseFailed: true}
	}
	defer f.Close()
	res := rules.Results{}
	dec := xml.NewDecoder(f)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Parsed{Results: res, ParseFailed: true}
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "testcase" {
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
		case len(c.Skipped) > 0:
			st = rules.Skipped
		}
		put(res, id, st)
	}
	return Parsed{Results: res}
}
