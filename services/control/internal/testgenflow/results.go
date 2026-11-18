package testgenflow

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
)

type PipelineResults struct {
	ChainsGitURL    string `json:"CHAINS-GIT_URL"`
	ChainsGitCommit string `json:"CHAINS-GIT_COMMIT"`
	TestsAdded      string `json:"TESTS_ADDED"`
	PRURL           string `json:"PR_URL"`
	RunRef          string `json:"RUN_REF"`
	EvidenceRef     string `json:"EVIDENCE_REF"`
	TerminalStatus  string `json:"terminal_status"`
}

type GenerationTaskResults struct {
	TestsAdded string `json:"TESTS_ADDED"`
	Attempts   string `json:"ATTEMPTS"`
	LastError  string `json:"LAST_ERROR"`
}

func (r Run) CompatibilityResults() (PipelineResults, GenerationTaskResults) {
	testsAdded := strconv.Itoa(r.TestsAdded)
	return PipelineResults{
			ChainsGitURL:    r.Request.RepositoryURL,
			ChainsGitCommit: r.ResolvedCommit,
			TestsAdded:      testsAdded,
			PRURL:           r.PRURL,
			RunRef:          r.RunID,
			EvidenceRef:     r.EvidenceRef,
			TerminalStatus:  string(r.Status),
		}, GenerationTaskResults{
			TestsAdded: testsAdded,
			Attempts:   strconv.Itoa(r.AttemptCount()),
			LastError:  r.LastError,
		}
}

func NewRunID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate test-generation run identity: %w", err)
	}
	return "testgen-run-" + hex.EncodeToString(raw[:]), nil
}

func NewEventID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate test-generation event identity: %w", err)
	}
	return "testgen-event-" + hex.EncodeToString(raw[:]), nil
}
