package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/project-lightpath/ai-test-generation/services/control/internal/testgensim"
)

func main() {
	scenario := flag.String("scenario", string(testgensim.ScenarioRetry), "success, retry, no-changes, or unknown-outcome")
	flag.Parse()

	report, err := testgensim.Run(context.Background(), testgensim.Scenario(*scenario))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
