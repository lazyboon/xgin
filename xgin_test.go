package xgin

import (
	"net/http"
	"testing"
)

func TestNormalizeStatus_ValidStatus(t *testing.T) {
	r := Response{HttpStatus: http.StatusCreated}
	if got := normalizeStatus(r); got != http.StatusCreated {
		t.Fatalf("expected %d, got %d", http.StatusCreated, got)
	}
}

func TestNormalizeStatus_DefaultJSON(t *testing.T) {
	r := Response{ContentType: ContentTypeJSON}
	if got := normalizeStatus(r); got != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, got)
	}
}

func TestNormalizeStatus_DefaultRedirect(t *testing.T) {
	r := Response{ContentType: ContentTypeRedirect}
	if got := normalizeStatus(r); got != http.StatusFound {
		t.Fatalf("expected %d, got %d", http.StatusFound, got)
	}
}

func TestSetValidatorMaxDepthAndSharedValidator(t *testing.T) {
	SetValidatorMaxDepth(32)
	v1 := SharedValidator(getValidatorMaxDepth())
	if v1 == nil {
		t.Fatal("expected validator instance")
	}

	SetValidatorMaxDepth(64)
	v2 := SharedValidator(getValidatorMaxDepth())
	if v2 == nil {
		t.Fatal("expected validator instance")
	}
	if v1 == v2 {
		t.Fatal("expected a new validator instance when depth changes")
	}

	SetValidatorMaxDepth(64)
	v3 := SharedValidator(getValidatorMaxDepth())
	if v2 != v3 {
		t.Fatal("expected same validator instance when depth is unchanged")
	}
}
