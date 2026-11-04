package prep

import (
	"fmt"
	"strings"

	"on-demand-zonal-stats/internal/release"
)

// QCResult is one release QC check, stored in the manifest.
type QCResult = release.QCResult

type qcLog struct{ results []QCResult }

func (q *qcLog) add(r QCResult) {
	q.results = append(q.results, r)
	status := "PASS"
	if !r.Passed {
		status = "FAIL"
		if !r.Blocking {
			status = "WARN"
		}
	}
	msg := fmt.Sprintf("qc %-28s %s", r.Name, status)
	if r.Value != nil {
		msg += fmt.Sprintf("  value=%v", r.Value)
	}
	if r.Error != "" {
		msg += "  error=" + r.Error
	}
	logf("%s", msg)
}

// info records a non-blocking, informational result.
func (q *qcLog) info(name string, value, detail any, err error) {
	r := QCResult{Name: name, Passed: err == nil, Value: value, Detail: detail}
	if err != nil {
		r.Error = err.Error()
	}
	q.add(r)
}

// failures lists the blocking checks that failed.
func (q *qcLog) failures() []string {
	var f []string
	for _, r := range q.results {
		if r.Blocking && !r.Passed {
			f = append(f, r.Name)
		}
	}
	return f
}

// BlockingError reports failed blocking QC checks.
type BlockingError struct{ Checks []string }

func (e *BlockingError) Error() string {
	return "prep: blocking QC checks failed: " + strings.Join(e.Checks, ", ")
}
