package report

import (
	"bytes"
	"encoding/csv"
	"slices"
	"strconv"
	"strings"
)

// CSVHeader is the column order of CSV; web/src/report.ts keeps a copy and a test compares the two.
var CSVHeader = []string{"namespace", "workload", "container", "pods", "cpu_request_m", "cpu_recommended_m",
	"mem_request_mib", "mem_recommended_mib", "monthly_saving_usd", "pod", "cpu_usage_m", "mem_usage_mib", "monthly_cost_usd"}

// safeCell stops a spreadsheet from running a text cell as a formula (spec 022).
func safeCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r\n", rune(s[0])) {
		return "'" + s
	}
	return s
}

// CSV renders the recommendations as one row per container: CRLF lines, UTF-8 without BOM, an estimate in dollars.
func CSV(recs []Rec) []byte {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	w.UseCRLF = true
	w.Write(CSVHeader)
	for _, r := range recs {
		w.Write(row(r, safeCell))
	}
	w.Flush()
	return b.Bytes()
}

// row is the cells of one recommendation in CSVHeader order; text escapes the cells that hold names.
// pod is an example pod of the container; usage is the peak over its pods; cost is today's monthly cost.
func row(r Rec, text func(string) string) []string {
	return []string{text(r.Namespace), text(r.Workload), text(r.Container), strconv.Itoa(r.Pods),
		strconv.FormatInt(r.Request.MilliCPU, 10), strconv.FormatInt(r.Recommended.MilliCPU, 10),
		strconv.FormatInt(r.Request.MemBytes>>20, 10), strconv.FormatInt(r.Recommended.MemBytes>>20, 10),
		strconv.FormatFloat(r.MonthlySaving, 'f', 2, 64), text(r.Pod), strconv.FormatInt(r.CPUUsage, 10),
		strconv.FormatInt(r.MemUsage>>20, 10), strconv.FormatFloat(r.MonthlyCost, 'f', 2, 64)}
}

// mdCell keeps a name inside its cell and stops it from becoming a link, code or HTML in a ticket.
var mdCell = strings.NewReplacer(`\`, `\\`, "|", `\|`, "<", `\<`, "[", `\[`, "`", "\\`", "\r\n", " ", "\n", " ", "\r", " ")

// Markdown renders the recommendations as a GitHub-flavored table with the columns of CSV.
func Markdown(recs []Rec) []byte {
	var b bytes.Buffer
	line := func(cells []string) { b.WriteString("| " + strings.Join(cells, " | ") + " |\n") }
	line(CSVHeader)
	line(slices.Repeat([]string{"---"}, len(CSVHeader)))
	for _, r := range recs {
		line(row(r, mdCell.Replace))
	}
	return b.Bytes()
}
