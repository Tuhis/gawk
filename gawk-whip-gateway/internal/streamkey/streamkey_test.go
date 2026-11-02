package streamkey

import "testing"

func TestParse(t *testing.T) {
	k, err := Parse("gawk1_k7q2xm_0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if k.ID != "K7Q2XM" || k.Token != "0123456789abcdef0123456789abcdef" || k.String() != "gawk1_K7Q2XM_0123456789abcdef0123456789abcdef" {
		t.Fatalf("%+v", k)
	}
	upper, _ := Parse("gawk1_K7Q2XM_0123456789abcdef0123456789abcdef")
	if upper.Hash != k.Hash {
		t.Fatal("the ID's case must not split a key's records")
	}
	other, _ := Parse("gawk1_K7Q2XM_0123456789abcdef0123456789abcdee")
	if other.Hash == k.Hash {
		t.Fatal("records are keyed by the full key, not the ID")
	}
	for _, bad := range []string{"", "gawk1_", "gawk1_K7Q2XM_", "gawk1_K7Q2XM0123456789abcdef0123456789abcdef",
		"gawk1_K7Q2XO_0123456789abcdef0123456789abcdef", "gawk1_ K7Q2X_0123456789abcdef0123456789abcdef",
		"gawk1_K7Q2XM_0123456789abcdef0123456789abcdeF", "GAWK1_K7Q2XM_0123456789abcdef0123456789abcdef",
		"gawk1_K7Q2XM_0123456789abcdef0123456789abcdef_"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if _, err := FromAuthorization("bearer gawk1_K7Q2XM_0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal("the scheme is case-insensitive")
	}
	if _, err := FromAuthorization("Basic gawk1_K7Q2XM_0123456789abcdef0123456789abcdef"); err == nil {
		t.Fatal("Basic accepted")
	}
}
