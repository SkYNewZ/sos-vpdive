// Package tickets holds the domain of members' requests: the catalog of
// categories and products, submission, the status matrix, reads, deletion,
// retention and the mails they cause (spec §3, §4, §6, §8).
package tickets

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SkYNewZ/sos-vpdive/internal/config"
)

// FieldType is the kind of a category field (spec §9.7).
type FieldType string

// Field types: short text, long text, single choice, date, number.
const (
	FieldText     FieldType = "text"
	FieldTextarea FieldType = "textarea"
	FieldChoice   FieldType = "choice"
	FieldDate     FieldType = "date"
	FieldNumber   FieldType = "number"
)

// Option is one value of a choice field, or one product.
type Option struct {
	ID    string `yaml:"id"`
	Label string `yaml:"label"`
}

// Field is a dedicated field of a category.
type Field struct {
	ID          string    `yaml:"id"`
	Label       string    `yaml:"label"`
	Hint        string    `yaml:"hint"`
	Type        FieldType `yaml:"type"`
	Required    bool      `yaml:"required"`
	Options     []Option  `yaml:"options"`      // choice: inline options, or the products once built
	OptionsFrom string    `yaml:"options_from"` // "products": Options is the product list
}

// Category groups requests and decides their dedicated fields.
type Category struct {
	ID            string  `yaml:"id"`
	Label         string  `yaml:"label"`
	Help          string  `yaml:"help"`
	CommitteeOnly bool    `yaml:"committee_only"`
	Fields        []Field `yaml:"fields"`
}

// Catalog is the content of config/categories.yaml and config/products.yaml.
type Catalog struct {
	Categories []Category // file order
	Products   []Option
}

// Fields is what a ticket stores, sealed as JSON.
type Fields struct {
	Category string            `json:"category"` // category id at submission
	Values   map[string]string `json:"values"`   // field id → raw value (option id for a choice)
}

// FieldValue is a stored field ready to display.
type FieldValue struct {
	Label   string
	Value   string // option or product label when known, else the raw value
	Removed bool   // field, option or product no longer in the catalog
}

const (
	categoriesPath = "config/categories.yaml"
	productsPath   = "config/products.yaml"
	productsSource = "products"
	removedSuffix  = " (retiré)"

	// Field limits (design §5).
	textMax     = 200
	textareaMax = 2000
	numberMax   = 9999
	dateDisplay = "02/01/2006"
)

// Ids are stable and end up in form input names: lowercase letters, digits
// and hyphens, so "_" can separate them in FieldName.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// LoadCatalog reads and validates the categories and products. Any problem
// prevents the start, naming the file and the entry at fault.
func LoadCatalog(content fs.FS) (*Catalog, error) {
	var pf struct {
		Products []Option `yaml:"products"`
	}
	if err := config.DecodeYAML(content, productsPath, &pf); err != nil {
		return nil, err
	}
	if err := validOptions(pf.Products); err != nil {
		return nil, fmt.Errorf("%s: products: %w", productsPath, err)
	}
	var cf struct {
		Categories []Category `yaml:"categories"`
	}
	if err := config.DecodeYAML(content, categoriesPath, &cf); err != nil {
		return nil, err
	}
	c := &Catalog{Products: pf.Products}
	var errs []error
	for i, cat := range cf.Categories {
		err := buildCategory(&cat, pf.Products)
		if _, dup := c.Category(cat.ID); err == nil && dup {
			err = fmt.Errorf("duplicate id %q", cat.ID)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: categories[%d]: %w", categoriesPath, i, err))
			continue
		}
		c.Categories = append(c.Categories, cat)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if len(c.Public()) == 0 {
		return nil, fmt.Errorf("%s: no category is offered on the form", categoriesPath)
	}
	return c, nil
}

// buildCategory validates cat and fills the options of its product fields.
func buildCategory(cat *Category, products []Option) error {
	if !idPattern.MatchString(cat.ID) {
		return fmt.Errorf("id %q must match %s", cat.ID, idPattern)
	}
	if strings.TrimSpace(cat.Label) == "" {
		return fmt.Errorf("category %q: empty label", cat.ID)
	}
	if cat.CommitteeOnly && len(cat.Fields) > 0 {
		return fmt.Errorf("category %q: a committee-only category has no fields", cat.ID)
	}
	cat.Help = strings.TrimSpace(cat.Help)
	seen := map[string]bool{}
	for j := range cat.Fields {
		f := &cat.Fields[j]
		err := buildField(f, products)
		if err == nil && seen[f.ID] {
			err = fmt.Errorf("duplicate id %q", f.ID)
		}
		if err != nil {
			return fmt.Errorf("category %q: fields[%d]: %w", cat.ID, j, err)
		}
		seen[f.ID] = true
	}
	return nil
}

func buildField(f *Field, products []Option) error {
	if !idPattern.MatchString(f.ID) {
		return fmt.Errorf("id %q must match %s", f.ID, idPattern)
	}
	if strings.TrimSpace(f.Label) == "" {
		return errors.New("empty label")
	}
	switch f.Type {
	case FieldText, FieldTextarea, FieldDate, FieldNumber:
		if len(f.Options) > 0 || f.OptionsFrom != "" {
			return errors.New("only a choice field takes options")
		}
	case FieldChoice:
		switch {
		case f.OptionsFrom == productsSource && len(f.Options) == 0:
			f.Options = products
		case f.OptionsFrom == "" && len(f.Options) > 0:
			return validOptions(f.Options)
		default:
			return fmt.Errorf("a choice field takes either options or options_from: %s", productsSource)
		}
	default:
		return fmt.Errorf("unknown type %q", f.Type)
	}
	return nil
}

func validOptions(opts []Option) error {
	if len(opts) == 0 {
		return errors.New("no option")
	}
	seen := map[string]bool{}
	for i, o := range opts {
		if !idPattern.MatchString(o.ID) || seen[o.ID] || strings.TrimSpace(o.Label) == "" {
			return fmt.Errorf("options[%d]: invalid or duplicate id, or empty label", i)
		}
		seen[o.ID] = true
	}
	return nil
}

// Category returns the category of id.
func (c *Catalog) Category(id string) (Category, bool) {
	if i := slices.IndexFunc(c.Categories, func(cat Category) bool { return cat.ID == id }); i >= 0 {
		return c.Categories[i], true
	}
	return Category{}, false
}

// Has reports whether id is a category.
func (c *Catalog) Has(id string) bool {
	_, ok := c.Category(id)
	return ok
}

// Public returns the categories offered on the form, in file order.
func (c *Catalog) Public() []Category {
	var out []Category
	for _, cat := range c.Categories {
		if !cat.CommitteeOnly {
			out = append(out, cat)
		}
	}
	return out
}

// CategoryLabel names a category, or marks an id that left the catalog.
func (c *Catalog) CategoryLabel(id string) string {
	if cat, ok := c.Category(id); ok {
		return cat.Label
	}
	return id + removedSuffix
}

// FieldName is the form input name of a field: "champ_<category>_<field>".
// Ids never contain "_", so the name is unambiguous.
func FieldName(categoryID, fieldID string) string {
	return "champ_" + categoryID + "_" + fieldID
}

// ReadFields validates the fields of categoryID from form values. errs maps a
// field id to a French message. Empty optional fields are left out of Values.
// An unknown category yields no value and no error: the caller checks it.
func (c *Catalog) ReadFields(categoryID string, get func(name string) string) (Fields, map[string]string) {
	f := Fields{Category: categoryID, Values: map[string]string{}}
	errs := map[string]string{}
	cat, ok := c.Category(categoryID)
	if !ok {
		return f, errs
	}
	for _, field := range cat.Fields {
		raw := strings.TrimSpace(normalizeNewlines(get(FieldName(categoryID, field.ID))))
		if raw == "" {
			if field.Required {
				errs[field.ID] = "Ce champ est obligatoire."
			}
			continue
		}
		value, msg := field.read(raw)
		if msg != "" {
			errs[field.ID] = msg
			continue
		}
		f.Values[field.ID] = value
	}
	return f, errs
}

// read checks a non-empty value; msg is a French error, empty when valid.
func (f Field) read(raw string) (value, msg string) {
	switch f.Type {
	case FieldText:
		if utf8.RuneCountInString(raw) > textMax || strings.Contains(raw, "\n") {
			return "", fmt.Sprintf("Une seule ligne de %d caractères au plus.", textMax)
		}
	case FieldTextarea:
		if utf8.RuneCountInString(raw) > textareaMax {
			return "", fmt.Sprintf("%d caractères au plus.", textareaMax)
		}
	case FieldNumber:
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > numberMax {
			return "", "Indique un nombre entier entre 0 et 9999."
		}
		return strconv.Itoa(n), ""
	case FieldDate:
		if _, err := time.Parse(time.DateOnly, raw); err != nil {
			return "", "Indique une date valide."
		}
	case FieldChoice:
		if _, ok := optionLabel(f.Options, raw); !ok {
			return "", "Choisis une valeur de la liste."
		}
	}
	return raw, ""
}

// Display lists the stored values: fields of the submission category in
// catalog order, then values whose field left the catalog, by id.
func (c *Catalog) Display(f Fields) []FieldValue {
	out := make([]FieldValue, 0, len(f.Values))
	shown := map[string]bool{}
	if cat, ok := c.Category(f.Category); ok {
		for _, field := range cat.Fields {
			if raw, ok := f.Values[field.ID]; ok {
				shown[field.ID] = true
				out = append(out, field.display(raw))
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(f.Values)) {
		if !shown[id] {
			out = append(out, FieldValue{Label: id, Value: f.Values[id], Removed: true})
		}
	}
	return out
}

func (f Field) display(raw string) FieldValue {
	v := FieldValue{Label: f.Label, Value: raw}
	switch f.Type {
	case FieldChoice:
		if label, ok := optionLabel(f.Options, raw); ok {
			v.Value = label
		} else {
			v.Removed = true
		}
	case FieldDate:
		if d, err := time.Parse(time.DateOnly, raw); err == nil {
			v.Value = d.Format(dateDisplay)
		}
	case FieldText, FieldTextarea, FieldNumber:
	}
	return v
}

// Product returns the label of the first value of a products field, "" if
// none or if that product left the catalog.
func (c *Catalog) Product(f Fields) string {
	cat, ok := c.Category(f.Category)
	if !ok {
		return ""
	}
	for _, field := range cat.Fields {
		raw, ok := f.Values[field.ID]
		if field.OptionsFrom != productsSource || !ok {
			continue
		}
		if label, ok := optionLabel(field.Options, raw); ok {
			return label
		}
	}
	return ""
}

func optionLabel(opts []Option, id string) (string, bool) {
	for _, o := range opts {
		if o.ID == id {
			return o.Label, true
		}
	}
	return "", false
}

// normalizeNewlines turns CRLF and lone CR into LF: browsers send CRLF.
func normalizeNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}
