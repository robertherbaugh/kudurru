package blocking

import (
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
)

func TestViewCatalogueIsOriginalBorrowedCatalog(t *testing.T) {
	var empty *View
	if empty.Catalogue() != nil || (&View{}).Catalogue() != nil {
		t.Fatal("empty view fabricated a catalog")
	}
	c, err := catalogue.Parse(catalogue.Embedded().Raw(), "https://original.example/catalog")
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewView(Options{}, time.Time{}, c)
	if err != nil {
		t.Fatal(err)
	}
	if v.Catalogue() != c || v.Catalogue().Origin() != c.Origin() {
		t.Fatal("view did not preserve its exact captured catalog")
	}
}
