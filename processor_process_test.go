package main

import (
	"context"
	"testing"
)

func TestProcess_SkipsUnsupportedAndDerived(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := &Processor{
		cfg: Config{
			ResizeTargets: []ResizeTarget{{Label: "w480", Width: 480}},
		},
		storage: nil,
	}
	if err := p.Process(ctx, storageEvent{Bucket: "b", Name: "a.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Process(ctx, storageEvent{Bucket: "b", Name: "images/a-w480.jpg"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Process(ctx, storageEvent{Bucket: "b", Name: "images/a-w2400.jpg"}); err != nil {
		t.Fatal(err)
	}
}

func TestProcess_NilStoragePanicsForSupportedImage(t *testing.T) {
	t.Parallel()
	var panicked bool
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		p := &Processor{
			cfg: Config{
				ResizeTargets: []ResizeTarget{{Label: "w480", Width: 480}},
			},
			storage: nil,
		}
		_ = p.Process(context.Background(), storageEvent{Bucket: "b", Name: "a.jpg"})
	}()
	if !panicked {
		t.Fatal("expected panic when storage client is nil for supported image")
	}
}
