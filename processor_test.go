package main

import "testing"

func TestIsSupportedImage(t *testing.T) {
	if !isSupportedImage("images/demo.JPG") {
		t.Fatal("expected JPG to be supported")
	}
	if !isSupportedImage("images/demo.webp") {
		t.Fatal("expected user-uploaded lowercase webp to be supported")
	}
	if isSupportedImage("images/demo.webP") {
		t.Fatal("expected generated mixed-case webP to be skipped")
	}
	if isSupportedImage("images/demo.txt") {
		t.Fatal("expected TXT to be unsupported")
	}
}

func TestIsDerivedObjectName(t *testing.T) {
	if !isDerivedObjectName("images/demo-w2400.jpg") {
		t.Fatal("expected w2400 resized image to be treated as derived")
	}
	if !isDerivedObjectName("images/demo-w800.jpg") {
		t.Fatal("expected resized image to be treated as derived")
	}
	if !isDerivedObjectName("images/demo-w480.webP") {
		t.Fatal("expected generated mixed-case webP to be treated as derived")
	}
	if isDerivedObjectName("images/demo.jpg") {
		t.Fatal("expected original image not to be treated as derived")
	}
}
