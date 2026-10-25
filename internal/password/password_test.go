package password

import (
	"strings"
	"testing"
)

func TestHashAndCheck(t *testing.T) {
	h := Hash("correct horse")
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("hash = %q", h)
	}
	if h == Hash("correct horse") {
		t.Fatal("two hashes share a salt")
	}
	if ok, err := Check(h, "correct horse"); !ok || err != nil {
		t.Fatalf("right password = %v, %v", ok, err)
	}
	if ok, err := Check(h, "correct horsf"); ok || err != nil {
		t.Fatalf("wrong password = %v, %v", ok, err)
	}
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$AA$AA", "$argon2id$v=19$m=x$AA$AA"} {
		if ok, err := Check(bad, "x"); ok || err == nil {
			t.Errorf("Check(%q) = %v, %v", bad, ok, err)
		}
	}
}
