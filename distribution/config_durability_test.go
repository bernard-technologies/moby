package distribution

import (
	"context"
	"testing"

	"github.com/docker/docker/image"
	digest "github.com/opencontainers/go-digest"
)

func TestImageConfigLookupRequiresRegisteredImage(t *testing.T) {
	backend, err := image.NewFSStoreBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := image.NewImageStore(backend, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers"}}`)
	id := digest.FromBytes(config)
	cache := NewImageConfigStoreFromStore(store)
	// A config can exist without an indexed image after a failed write or
	// when its layer chain was not restorable.
	if _, err := backend.Set(config); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Get(context.Background(), id); err == nil {
		t.Fatal("lookup would skip the pull for an unregistered image")
	}
	if _, err := cache.Put(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Get(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}
