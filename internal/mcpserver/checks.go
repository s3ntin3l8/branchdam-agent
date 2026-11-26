package mcpserver

import "fmt"

// CheckResult is one preflight check. Status is "OK", "WARN", or "FAIL".
// OK is false only for FAIL: a warning does not fail the report. Detail
// is the same sentence the text printer writes after the status.
type CheckResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// String is the preflight text line. The width matches the historical
// "[OK  ]" / "[WARN]" / "[FAIL]" padding so the command's output stays
// byte-identical.
func (c CheckResult) String() string {
	return fmt.Sprintf("[%-4s] %s", c.Status, c.Detail)
}

// NewCheck builds a CheckResult. ok is derived from status.
func NewCheck(name, status, detail string) CheckResult {
	return CheckResult{
		Name:   name,
		OK:     status != "FAIL",
		Status: status,
		Detail: detail,
	}
}
