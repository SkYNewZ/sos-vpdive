package ffessm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAPI serves the search and detail endpoints with one synthetic licensee
// and records the search texts it received.
func fakeAPI(t *testing.T) (*Client, *[]string) {
	t.Helper()
	var texts []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /V1/Utilisateurs/Utilisateur/Search", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SearchText string `json:"SearchText"`
		}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		texts = append(texts, body.SearchText)
		items := []map[string]any{}
		if body.SearchText == "dupont marie" || body.SearchText == "A-26-000001" {
			items = append(items,
				map[string]any{"Id": "lic-1", "Type": "Licencie", "NumeroLicence": "A-26-000001", "Nom": "DUPONT MARIE",
					"DateDeNaissance": "1990-01-02", "DateValiditeLicence": "2026-12-31T00:00:00", "NomStructure": "CLUB TEST",
					"EstActif": true, "EstMoniteur": false},
				map[string]any{"Id": "str-1", "Type": "Structure", "Nom": "DUPONT PLONGEE"})
		}
		writeJSON(t, w, map[string]any{"Items": items})
	})
	mux.HandleFunc("POST /V1/Utilisateurs/Licencie/ListByEmail", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "marie@example.org", body["searchText"])
		writeJSON(t, w, []map[string]any{{"Id": "lic-1", "NumeroLicence": "A-26-000001", "NomPrenom": "DUPONT Marie",
			"Email": "marie@example.org", "NomStructure": "CLUB TEST"}})
	})
	mux.HandleFunc("GET /V1/Utilisateurs/Licencie/lic-1", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		writeJSON(t, w, map[string]any{"Adresse": map[string]any{"Email": "marie@example.org", "TelephonePortable": "0600000000",
			"Adresse1": "1 rue de la Mer", "CodePostal": "83000", "Commune": "Toulon"}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return signedIn(srv), &texts
}

// signedIn returns a client holding a valid token: these tests cover the API side.
func signedIn(srv *httptest.Server) *Client {
	return &Client{api: srv.URL, http: srv.Client(), token: "test-token", expiry: time.Now().Add(time.Hour)}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	assert.NoError(t, json.NewEncoder(w).Encode(v))
}

func TestSearchRejectsShortQuery(t *testing.T) {
	c, _ := fakeAPI(t)
	_, err := c.Search(context.Background(), " ab ")
	assert.ErrorIs(t, err, ErrQueryTooShort)
}

func TestSearchByNameKeepsLicenseesWithContactDetails(t *testing.T) {
	c, _ := fakeAPI(t)
	got, err := c.Search(context.Background(), "  dupont   marie ")
	require.NoError(t, err)
	assert.Equal(t, []Licence{{
		Number: "A-26-000001", Name: "DUPONT MARIE", BirthDate: "1990-01-02", ValidUntil: "2026-12-31",
		Club: "CLUB TEST", Active: true, Email: "marie@example.org", Mobile: "0600000000",
		Address: "1 rue de la Mer, 83000 Toulon",
	}}, got)
}

func TestSearchRetriesNameInOtherOrder(t *testing.T) {
	c, texts := fakeAPI(t)
	got, err := c.Search(context.Background(), "marie dupont")
	require.NoError(t, err)
	assert.Equal(t, []string{"marie dupont", "dupont marie"}, *texts)
	require.Len(t, got, 1)
	assert.Equal(t, "A-26-000001", got[0].Number)
}

func TestSearchByLicenceNumberDoesNotRetry(t *testing.T) {
	c, texts := fakeAPI(t)
	got, err := c.Search(context.Background(), "A-26-000001")
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Equal(t, []string{"A-26-000001"}, *texts)
}

func TestSearchByEmailUsesEmailEndpoint(t *testing.T) {
	c, texts := fakeAPI(t)
	got, err := c.Search(context.Background(), "marie@example.org")
	require.NoError(t, err)
	assert.Empty(t, *texts)
	require.Len(t, got, 1)
	assert.Equal(t, "DUPONT Marie", got[0].Name)
	assert.Equal(t, "marie@example.org", got[0].Email)
}

func TestSearchErrorHidesLicenseeID(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /V1/Utilisateurs/Utilisateur/Search", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"Items": []map[string]any{{"Id": "secret-uuid", "Type": "Licencie"}}})
	})
	mux.HandleFunc("GET /V1/Utilisateurs/Licencie/secret-uuid", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	_, err := signedIn(srv).Search(context.Background(), "dupont marie")
	require.ErrorIs(t, err, ErrUnexpected)
	assert.NotContains(t, err.Error(), "secret-uuid")

	srv.Close() // a transport error carries the URL too
	_, err = signedIn(srv).Search(context.Background(), "dupont marie")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), srv.URL)
}

// noIdentity returns the URL of a closed server: any sign-in attempt fails.
func noIdentity(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.URL
}

func TestSearchReusesTokenUntilExpiry(t *testing.T) {
	c, texts := fakeAPI(t)
	c.identity = noIdentity(t)
	for range 2 {
		_, err := c.Search(context.Background(), "dupont marie")
		require.NoError(t, err, "a valid token needs no sign-in")
	}
	assert.Len(t, *texts, 2)
	assert.Equal(t, "test-token", c.token)
}

func TestSearchSignsInAgainOnceTokenExpired(t *testing.T) {
	c, texts := fakeAPI(t)
	c.identity = noIdentity(t)
	c.expiry = time.Now().Add(-time.Second)
	_, err := c.Search(context.Background(), "dupont marie")
	require.ErrorContains(t, err, "ffessm: login")
	assert.Empty(t, *texts, "an expired token is never sent")
}

func TestSearchSignsInAgainOnUnauthorized(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	c := signedIn(srv)
	c.identity = noIdentity(t)
	_, err := c.Search(context.Background(), "dupont marie")
	require.ErrorContains(t, err, "ffessm: login", "a refused token triggers a new sign-in")
	assert.Equal(t, 1, calls)
	assert.Empty(t, c.token, "the refused token is forgotten")
}

func TestSearchReportsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	_, err := signedIn(srv).Search(context.Background(), "dupont marie")
	assert.ErrorIs(t, err, ErrUnexpected)
}
