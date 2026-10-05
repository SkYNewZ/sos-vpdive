package web

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strings"

	"go.yaml.in/yaml/v3"
)

// decodeStrict reads a YAML content file, refusing unknown keys.
func decodeStrict(content fs.FS, name string, v any) error {
	data, err := fs.ReadFile(content, name)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	return nil
}

// robotsPolicy holds the AI robots refused by robots.txt and by user agent.
type robotsPolicy struct {
	names      []string // every name, for robots.txt
	userAgents []string // lowercase names that arrive as user agents
}

func loadRobots(content fs.FS) (robotsPolicy, error) {
	var f struct {
		Robots []struct {
			Name          string `yaml:"name"`
			RobotsTxtOnly bool   `yaml:"robots_txt_only"`
		} `yaml:"robots"`
	}
	if err := decodeStrict(content, "config/robots.yaml", &f); err != nil {
		return robotsPolicy{}, err
	}
	if len(f.Robots) == 0 {
		return robotsPolicy{}, errors.New("config/robots.yaml: no robot listed")
	}
	var p robotsPolicy
	seen := map[string]bool{}
	for i, r := range f.Robots {
		name := strings.TrimSpace(r.Name)
		if name == "" || strings.ContainsAny(name, "\r\n:") || seen[strings.ToLower(name)] {
			return robotsPolicy{}, fmt.Errorf("config/robots.yaml: robots[%d]: empty, invalid or duplicate name", i)
		}
		seen[strings.ToLower(name)] = true
		p.names = append(p.names, name)
		if !r.RobotsTxtOnly {
			p.userAgents = append(p.userAgents, strings.ToLower(name))
		}
	}
	return p, nil
}

func (p robotsPolicy) refuses(userAgent string) bool {
	ua := strings.ToLower(userAgent)
	for _, n := range p.userAgents {
		if strings.Contains(ua, n) {
			return true
		}
	}
	return false
}

// txt renders robots.txt: AI robots refused by name; search engines allowed,
// since a blocked engine would never read the noindex header (spec §11.8).
func (p robotsPolicy) txt() string {
	var b strings.Builder
	b.WriteString("# AI crawlers are refused. Search engines may crawl:\n")
	b.WriteString("# every response carries X-Robots-Tag: noindex.\n")
	for _, n := range p.names {
		b.WriteString("User-agent: " + n + "\n")
	}
	b.WriteString("Disallow: /\n\nUser-agent: *\nAllow: /\n")
	return b.String()
}

// vpdiveLink is a button that opens VPDive in a new tab (spec §7).
type vpdiveLink struct {
	Label string
	URL   string
}

type vpdiveLinks map[string]vpdiveLink

// requiredLinks are the keys the code relies on.
var requiredLinks = []string{"membres"}

func loadVPDiveLinks(content fs.FS, base *url.URL) (vpdiveLinks, error) {
	var f struct {
		Links []struct {
			Key   string `yaml:"key"`
			Label string `yaml:"label"`
			Path  string `yaml:"path"`
		} `yaml:"links"`
	}
	if err := decodeStrict(content, "config/vpdive.yaml", &f); err != nil {
		return nil, err
	}
	links := vpdiveLinks{}
	for i, l := range f.Links {
		if l.Key == "" || strings.TrimSpace(l.Label) == "" || !strings.HasPrefix(l.Path, "/") {
			return nil, fmt.Errorf("config/vpdive.yaml: links[%d]: key, label and a path starting with / are required", i)
		}
		if _, dup := links[l.Key]; dup {
			return nil, fmt.Errorf("config/vpdive.yaml: links[%d]: duplicate key %q", i, l.Key)
		}
		links[l.Key] = vpdiveLink{Label: l.Label, URL: base.String() + l.Path}
	}
	for _, k := range requiredLinks {
		if _, ok := links[k]; !ok {
			return nil, fmt.Errorf("config/vpdive.yaml: missing link %q", k)
		}
	}
	return links, nil
}
