package report

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseJUnitReportPreservesTestIdentityAndOutcomes(t *testing.T) {
	data := []byte(`<?xml version="1.0"?>
<testsuites><testsuite name="unit">
  <testcase classname="pkg.PassingTest" name="passes"/>
  <testcase classname="pkg.FailingTest" name="fails"><failure message="assertion failed">details</failure></testcase>
  <testcase classname="pkg.SkippedTest" name="skips"><skipped/></testcase>
  <testcase classname="pkg.OtherTest" name="unexpected"/>
</testsuite></testsuites>`)

	summary, err := Parse(JUnitMediaType, data, []string{
		"pkg.PassingTest#passes", "pkg.FailingTest#fails", "pkg.SkippedTest#skips", "pkg.MissingTest#missing",
	}, 4096)
	if err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if summary.MediaType != JUnitMediaType || summary.EvidenceComplete {
		t.Fatalf("media type or completeness = %#v", summary)
	}
	assertIDs(t, summary.ReportedTestIDs, "pkg.FailingTest#fails", "pkg.OtherTest#unexpected", "pkg.PassingTest#passes", "pkg.SkippedTest#skips")
	assertIDs(t, summary.ExecutedTestIDs, "pkg.FailingTest#fails", "pkg.OtherTest#unexpected", "pkg.PassingTest#passes")
	assertIDs(t, summary.SkippedTestIDs, "pkg.SkippedTest#skips")
	assertIDs(t, summary.FailedTestIDs, "pkg.FailingTest#fails")
	assertIDs(t, summary.MissingTestIDs, "pkg.MissingTest#missing")
	assertIDs(t, summary.UnexpectedTestIDs, "pkg.OtherTest#unexpected")
}

func TestParseJUnitReportCompleteEvidenceIsNotVerdict(t *testing.T) {
	data := []byte(`<testsuite><testcase classname="pkg.Test" name="runs"/></testsuite>`)
	summary, err := Parse(JUnitMediaType, data, []string{"pkg.Test#runs"}, 1024)
	if err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if !summary.EvidenceComplete || len(summary.FailedTestIDs) != 0 || len(summary.ExecutedTestIDs) != 1 {
		t.Fatalf("report summary = %#v", summary)
	}
}

func TestParseRejectsUnsupportedAndUnboundedInput(t *testing.T) {
	data := []byte(`<testsuite><testcase classname="pkg.Test" name="runs"/></testsuite>`)
	if _, err := Parse("application/json", data, []string{"pkg.Test#runs"}, 1024); !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("unsupported media type error = %v", err)
	}
	if _, err := Parse(JUnitMediaType, data, []string{"pkg.Test#runs"}, int64(len(data)-1)); !errors.Is(err, ErrReportTooLarge) {
		t.Fatalf("oversize error = %v", err)
	}
	if _, err := Parse(JUnitMediaType, data, nil, 1024); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("empty expected inventory error = %v", err)
	}
	if _, err := Parse(JUnitMediaType, nil, []string{"pkg.Test#runs"}, 1024); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("empty report error = %v", err)
	}
}

func TestParseRejectsUnsafeOrAmbiguousXML(t *testing.T) {
	validCase := `<testcase classname="pkg.Test" name="runs"/>`
	tests := []struct {
		name string
		xml  string
	}{
		{name: "doctype", xml: `<!DOCTYPE testsuite [<!ENTITY x "y">]><testsuite>` + validCase + `</testsuite>`},
		{name: "duplicate identity", xml: `<testsuite>` + validCase + validCase + `</testsuite>`},
		{name: "conflicting outcomes", xml: `<testsuite><testcase classname="pkg.Test" name="runs"><failure/><skipped/></testcase></testsuite>`},
		{name: "nested testcase", xml: `<testsuite><testcase classname="pkg.Test" name="runs">` + validCase + `</testcase></testsuite>`},
		{name: "nested failure", xml: `<testsuite><testcase classname="pkg.Test" name="runs"><system-out><failure/></system-out></testcase></testsuite>`},
		{name: "unknown testcase child", xml: `<testsuite><testcase classname="pkg.Test" name="runs"><result status="passed"/></testcase></testsuite>`},
		{name: "malformed document", xml: `<testsuite>` + validCase},
		{name: "no test cases", xml: `<testsuite/>`},
		{name: "processing instruction", xml: `<?unexpected data?><testsuite>` + validCase + `</testsuite>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Parse(JUnitMediaType, []byte(test.xml), []string{"pkg.Test#runs"}, 4096); !errors.Is(err, ErrInvalidReport) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestParseRejectsExcessiveXMLDepth(t *testing.T) {
	data := `<testsuite>` + strings.Repeat("<x>", MaxXMLDepth) + strings.Repeat("</x>", MaxXMLDepth) + `</testsuite>`
	if _, err := Parse(JUnitMediaType, []byte(data), []string{"pkg.Test#runs"}, 4096); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("depth error = %v", err)
	}
}

func TestParseRejectsDuplicateExpectedIdentities(t *testing.T) {
	data := []byte(`<testsuite><testcase classname="pkg.Test" name="runs"/></testsuite>`)
	if _, err := Parse(JUnitMediaType, data, []string{"pkg.Test#runs", "pkg.Test#runs"}, 1024); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("duplicate expected identity error = %v", err)
	}
}

func assertIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs = %#v, want %#v", got, want)
	}
}
