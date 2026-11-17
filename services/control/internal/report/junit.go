package report

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	JUnitMediaType = "application/xml"
	MaxReportBytes = 10_000_000
	MaxReportTests = 10_000
	MaxXMLDepth    = 32
	MaxTestIDBytes = 1_024
)

var (
	ErrUnsupportedMediaType = errors.New("unsupported test report media type")
	ErrInvalidReport        = errors.New("invalid test report")
	ErrReportTooLarge       = errors.New("test report exceeds size limit")
)

// Summary records identities and report observations only. It never translates
// a zero exit or an empty failure list into a test-pass verdict.
type Summary struct {
	MediaType         string   `json:"media_type"`
	ReportedTestIDs   []string `json:"reported_test_ids"`
	ExecutedTestIDs   []string `json:"executed_test_ids"`
	SkippedTestIDs    []string `json:"skipped_test_ids"`
	FailedTestIDs     []string `json:"failed_test_ids"`
	MissingTestIDs    []string `json:"missing_test_ids"`
	UnexpectedTestIDs []string `json:"unexpected_test_ids"`
	EvidenceComplete  bool     `json:"evidence_complete"`
}

type testCase struct {
	id      string
	skipped bool
	failed  bool
}

// Parse selects only a parser explicitly named by the approved profile's report
// media type. Paths, command execution, and application-specific verdicts are
// deliberately outside this package.
func Parse(mediaType string, data []byte, expectedTestIDs []string, maxBytes int64) (Summary, error) {
	if mediaType != JUnitMediaType {
		return Summary{}, ErrUnsupportedMediaType
	}
	if maxBytes <= 0 || maxBytes > MaxReportBytes {
		maxBytes = MaxReportBytes
	}
	if len(data) == 0 {
		return Summary{}, fmt.Errorf("%w: report is empty", ErrInvalidReport)
	}
	if int64(len(data)) > maxBytes {
		return Summary{}, ErrReportTooLarge
	}
	if len(expectedTestIDs) == 0 || len(expectedTestIDs) > MaxReportTests {
		return Summary{}, fmt.Errorf("%w: expected test identity inventory is empty or oversized", ErrInvalidReport)
	}
	expected := make(map[string]struct{}, len(expectedTestIDs))
	expectedBytes := 0
	for _, testID := range expectedTestIDs {
		if testID == "" || len(testID) > MaxTestIDBytes || strings.ContainsAny(testID, "\x00\t\n\v\f\r") {
			return Summary{}, fmt.Errorf("%w: expected test ID is empty or oversized", ErrInvalidReport)
		}
		expectedBytes += len(testID)
		if expectedBytes > 500_000 {
			return Summary{}, fmt.Errorf("%w: expected test identity inventory exceeds byte limit", ErrInvalidReport)
		}
		if _, duplicate := expected[testID]; duplicate {
			return Summary{}, fmt.Errorf("%w: duplicate expected test ID", ErrInvalidReport)
		}
		expected[testID] = struct{}{}
	}
	observed, err := parseJUnit(data)
	if err != nil {
		return Summary{}, err
	}
	if len(observed) == 0 {
		return Summary{}, fmt.Errorf("%w: report contains no test cases", ErrInvalidReport)
	}
	if len(observed) > MaxReportTests {
		return Summary{}, fmt.Errorf("%w: test case count exceeds limit", ErrInvalidReport)
	}

	summary := Summary{MediaType: mediaType}
	reported := make(map[string]struct{}, len(observed))
	for _, test := range observed {
		summary.ReportedTestIDs = append(summary.ReportedTestIDs, test.id)
		reported[test.id] = struct{}{}
		if _, isExpected := expected[test.id]; !isExpected {
			summary.UnexpectedTestIDs = append(summary.UnexpectedTestIDs, test.id)
		}
		if test.skipped {
			summary.SkippedTestIDs = append(summary.SkippedTestIDs, test.id)
		} else {
			summary.ExecutedTestIDs = append(summary.ExecutedTestIDs, test.id)
			if test.failed {
				summary.FailedTestIDs = append(summary.FailedTestIDs, test.id)
			}
		}
	}
	for testID := range expected {
		if _, found := reported[testID]; !found {
			summary.MissingTestIDs = append(summary.MissingTestIDs, testID)
		}
	}
	sort.Strings(summary.ReportedTestIDs)
	sort.Strings(summary.ExecutedTestIDs)
	sort.Strings(summary.SkippedTestIDs)
	sort.Strings(summary.FailedTestIDs)
	sort.Strings(summary.MissingTestIDs)
	sort.Strings(summary.UnexpectedTestIDs)
	summary.EvidenceComplete = len(summary.MissingTestIDs) == 0 && len(summary.UnexpectedTestIDs) == 0
	return summary, nil
}

func parseJUnit(data []byte) ([]testCase, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.Strict = true
	var stack []xml.Name
	var cases []testCase
	seenCases := make(map[string]struct{})
	var active *testCase
	activeDepth := 0
	rootSeen := false
	rootClosed := false
	xmlDeclarationSeen := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: parse XML: %v", ErrInvalidReport, err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if rootClosed || len(stack) >= MaxXMLDepth {
				return nil, fmt.Errorf("%w: XML root or nesting depth is invalid", ErrInvalidReport)
			}
			if len(stack) == 0 {
				if rootSeen || value.Name.Local != "testsuite" && value.Name.Local != "testsuites" {
					return nil, fmt.Errorf("%w: root must be testsuite or testsuites", ErrInvalidReport)
				}
				rootSeen = true
			}
			if value.Name.Local == "testcase" {
				if active != nil {
					return nil, fmt.Errorf("%w: nested testcase elements are invalid", ErrInvalidReport)
				}
				if len(stack) == 0 || stack[len(stack)-1].Local != "testsuite" {
					return nil, fmt.Errorf("%w: testcase must be directly contained by testsuite", ErrInvalidReport)
				}
				id, err := testcaseID(value)
				if err != nil {
					return nil, err
				}
				active = &testCase{id: id}
				activeDepth = len(stack) + 1
			} else if active != nil {
				if len(stack)+1 != activeDepth+1 {
					return nil, fmt.Errorf("%w: nested testcase report elements are invalid", ErrInvalidReport)
				}
				switch value.Name.Local {
				case "failure", "error":
					active.failed = true
				case "skipped":
					active.skipped = true
				case "system-out", "system-err":
				default:
					return nil, fmt.Errorf("%w: unsupported testcase report element %q", ErrInvalidReport, value.Name.Local)
				}
			}
			stack = append(stack, value.Name)
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1] != value.Name {
				return nil, fmt.Errorf("%w: mismatched XML element", ErrInvalidReport)
			}
			if value.Name.Local == "testcase" && active != nil && len(stack) == activeDepth {
				if active.failed && active.skipped {
					return nil, fmt.Errorf("%w: testcase cannot be both skipped and failed", ErrInvalidReport)
				}
				if _, duplicate := seenCases[active.id]; duplicate {
					return nil, fmt.Errorf("%w: duplicate reported test identity %q", ErrInvalidReport, active.id)
				}
				if len(cases) >= MaxReportTests {
					return nil, fmt.Errorf("%w: test case count exceeds limit", ErrInvalidReport)
				}
				seenCases[active.id] = struct{}{}
				cases = append(cases, *active)
				active = nil
				activeDepth = 0
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				rootClosed = true
			}
		case xml.Directive:
			return nil, fmt.Errorf("%w: XML directives and DTDs are forbidden", ErrInvalidReport)
		case xml.ProcInst:
			if value.Target != "xml" || rootSeen || xmlDeclarationSeen {
				return nil, fmt.Errorf("%w: XML processing instructions are forbidden", ErrInvalidReport)
			}
			xmlDeclarationSeen = true
		case xml.CharData:
			if len(stack) == 0 && strings.TrimSpace(string(value)) != "" {
				return nil, fmt.Errorf("%w: non-whitespace outside XML root", ErrInvalidReport)
			}
		}
	}
	if !rootSeen || !rootClosed || len(stack) != 0 || active != nil {
		return nil, fmt.Errorf("%w: incomplete XML document", ErrInvalidReport)
	}
	return cases, nil
}

func testcaseID(start xml.StartElement) (string, error) {
	attributes := make(map[string]string, len(start.Attr))
	for _, attribute := range start.Attr {
		if attribute.Name.Space != "" {
			continue
		}
		if _, duplicate := attributes[attribute.Name.Local]; duplicate {
			return "", fmt.Errorf("%w: duplicate testcase attribute", ErrInvalidReport)
		}
		attributes[attribute.Name.Local] = attribute.Value
	}
	className, name := attributes["classname"], attributes["name"]
	if className == "" || name == "" || strings.Contains(className, "#") || len(className)+len(name)+1 > MaxTestIDBytes || strings.ContainsAny(className+name, "\x00\t\n\v\f\r") {
		return "", fmt.Errorf("%w: testcase identity is missing or invalid", ErrInvalidReport)
	}
	return className + "#" + name, nil
}
