package web

// formModel identifies the independent model calls made by the member form.
type formModel string

const (
	formModelSuggest formModel = "suggest"
	formModelSummary formModel = "summary"
)
