package hisepress

import (
	"encoding/xml"
	"io"
)

type junitReport struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Errors   int          `xml:"errors,attr"`
	Skipped  int          `xml:"skipped,attr"`
	Time     float64      `xml:"time,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name      string      `xml:"name,attr"`
	Tests     int         `xml:"tests,attr"`
	Failures  int         `xml:"failures,attr"`
	Errors    int         `xml:"errors,attr"`
	Skipped   int         `xml:"skipped,attr"`
	Time      float64     `xml:"time,attr"`
	Timestamp string      `xml:"timestamp,attr"`
	Cases     []junitCase `xml:"testcase"`
	SystemOut string      `xml:"system-out,omitempty"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      float64       `xml:"time,attr"`
	Failure   *junitProblem `xml:"failure,omitempty"`
	Error     *junitProblem `xml:"error,omitempty"`
	Skipped   *struct{}     `xml:"skipped,omitempty"`
}

type junitProblem struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

// WriteJUnit writes results as JUnit XML: one testsuite per suite, one
// testcase per step. Failed steps become <failure>, errored ones <error>.
func WriteJUnit(w io.Writer, results []*Result) error {
	rep := junitReport{}
	for _, r := range results {
		js := junitSuite{
			Name:      r.Suite,
			Time:      float64(r.DurationMS) / 1000,
			Timestamp: r.Start.UTC().Format("2006-01-02T15:04:05"),
			SystemOut: r.Log,
		}
		for _, s := range r.Steps {
			c := junitCase{Name: s.Name, Classname: r.Suite, Time: float64(s.DurationMS) / 1000}
			switch s.Status {
			case StatusFail:
				c.Failure = &junitProblem{Message: s.Error, Text: s.Error}
				js.Failures++
			case StatusError:
				c.Error = &junitProblem{Message: s.Error, Text: s.Error}
				js.Errors++
			case StatusSkipped:
				c.Skipped = &struct{}{}
				js.Skipped++
			}
			js.Tests++
			js.Cases = append(js.Cases, c)
		}
		rep.Tests += js.Tests
		rep.Failures += js.Failures
		rep.Errors += js.Errors
		rep.Skipped += js.Skipped
		rep.Time += js.Time
		rep.Suites = append(rep.Suites, js)
	}
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}
