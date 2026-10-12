package internalapi

import (
	"context"
	"errors"
	"testing"
)

func TestServerCatalogue(t *testing.T) {
	body := []byte(`{"schemaVersion":1}`)
	n := startNode(t, CredentialOptions{Key: key(9), NodeName: "node-a"}, ServerOptions{
		Catalogue: func() CatalogueBody { return CatalogueBody{Body: body, Source: "https://data.example/catalogue.json"} },
	})
	got, err := n.client.Catalogue(context.Background(), n.addr)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Body) != string(body) || got.Source != "https://data.example/catalogue.json" {
		t.Fatalf("catalogue %q from %q", got.Body, got.Source)
	}
	old := startNode(t, CredentialOptions{Key: key(9), NodeName: "node-c"}, ServerOptions{})
	if _, err := old.client.Catalogue(context.Background(), old.addr); !errors.Is(err, ErrCatalogueUnsupported) {
		t.Fatalf("without Catalogue: %v", err)
	}
}
