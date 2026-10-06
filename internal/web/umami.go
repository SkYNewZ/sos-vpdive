package web

import "strings"

// umamiPage is what app.js needs to count a page view with Umami (spec
// §9.10).
type umamiPage struct {
	Src     string // the script, loaded by app.js only without Do Not Track
	Website string // the site's Umami ID
	Path    string // the route template, sent instead of the real address
}

// umamiPage returns the page-view settings for a page of the route pattern,
// or nil when its site is not measured or the page has no route of its own.
func (s *Server) umamiPage(pattern string, admin bool) *umamiPage {
	u := s.cfg.Umami
	if u == nil {
		return nil
	}
	website := u.WebsiteID
	if admin {
		website = u.AdminWebsiteID
	}
	path := umamiPath(pattern)
	if website == "" || path == "" {
		return nil
	}
	return &umamiPage{Src: u.ScriptURL.String(), Website: website, Path: path}
}

// umamiParams writes path wildcards the way spec §9.10 shows them: the
// tracking token masked, any other wildcard by its name.
var umamiParams = strings.NewReplacer("{$}", "", "{jeton}", "[masqué]", "{", "[", "}", "]")

// umamiPath turns a route pattern into the address Umami receives, never the
// real one: "GET /suivi/{jeton}" gives "/suivi/[masqué]". The catch-all,
// which has no method and matches any path, gives "".
func umamiPath(pattern string) string {
	_, path, found := strings.Cut(pattern, " ")
	if !found {
		return ""
	}
	return umamiParams.Replace(path)
}
