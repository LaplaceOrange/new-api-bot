package secure

import (
	"bytes"
	"testing"
)

func TestBox(t *testing.T) {
	box, err := New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	mac := box.MAC("bind:user", "123456")
	if !box.VerifyMAC("bind:user", "123456", mac) {
		t.Fatal("MAC verification failed")
	}
	if box.VerifyMAC("bind:user", "654321", mac) {
		t.Fatal("wrong value verified")
	}
	ciphertext, err := box.Encrypt("redemption-code")
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := box.Decrypt(ciphertext)
	if err != nil || plaintext != "redemption-code" {
		t.Fatalf("decrypt=%q err=%v", plaintext, err)
	}
}
