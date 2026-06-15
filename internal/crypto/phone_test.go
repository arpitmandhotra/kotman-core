package crypto

import "testing"

func TestHashPhone(t *testing.T) {
    // All of these must produce the same hash
    inputs := []string{
        "+91 98765 43210",
        "+919876543210",
        "09876543210",
        "9876543210",
        "91 9876543210",
    }

    expected := HashPhone("+919876543210") // canonical reference

    for _, input := range inputs {
        got := HashPhone(input)
        if got != expected {
            t.Errorf("HashPhone(%q) = %q, want %q", input, got, expected)
        }
    }
}

func TestHashPhone_DifferentNumbers_DifferentHashes(t *testing.T) {
    h1 := HashPhone("9876543210")
    h2 := HashPhone("9876543211") // one digit different
    if h1 == h2 {
        t.Error("different phone numbers produced the same hash")
    }
}