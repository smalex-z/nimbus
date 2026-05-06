package s3storage

import (
	"strings"
	"testing"
)

func TestComputePrefix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		input  string
		userID uint
		want   string
	}{
		{"plain ascii", "Kevin", 3, "kevin-u3"},
		{"with email shape", "kevin.sun@example.com", 7, "kevin-sun-ex-u7"},
		{"caps + spaces", "Kevin Sun", 12, "kevin-sun-u12"},
		{"unicode only", "李文", 42, "u42"},
		{"unicode mixed", "Kevin 李", 5, "kevin-u5"},
		{"empty", "", 9, "u9"},
		{"hyphens already", "k--evin", 1, "k-evin-u1"},
		{"leading hyphens stripped", "---kevin", 4, "kevin-u4"},
		{"trailing hyphens stripped", "kevin---", 6, "kevin-u6"},
		{"too long body capped", "kevinkevinkevinkevin", 8, "kevinkevinke-u8"},
		{"digits ok", "kevin99", 11, "kevin99-u11"},
		{"all unicode email", "李@example.com", 13, "example-com-u13"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computePrefix(tc.input, tc.userID)
			if got != tc.want {
				t.Errorf("computePrefix(%q, %d) = %q; want %q", tc.input, tc.userID, got, tc.want)
			}
			// Belt-and-suspenders: every prefix must be a valid bucket name fragment.
			// Lowercase ascii letters/digits/hyphens, no leading/trailing hyphen,
			// length sane.
			if got == "" {
				t.Errorf("empty prefix")
			}
			if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
				t.Errorf("prefix %q starts or ends with hyphen", got)
			}
			for _, c := range got {
				if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
					t.Errorf("prefix %q contains invalid char %q", got, c)
				}
			}
		})
	}
}

func TestValidUserPart(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  bool
	}{
		{"abc", true},
		{"my-app", true},
		{"my-app-uploads", true},
		{"123", true},
		{"a1-b2", true},
		{"ab", false},                    // too short
		{strings.Repeat("a", 31), false}, // too long
		{"-abc", false},                  // leading hyphen
		{"abc-", false},                  // trailing hyphen
		{"ABC", false},                   // uppercase
		{"my_app", false},                // underscore
		{"my.app", false},                // dot
		{"my app", false},                // space
		{"my/app", false},                // slash
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := validUserPart(tc.input)
			if got != tc.want {
				t.Errorf("validUserPart(%q) = %v; want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestValidComposedName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  bool
	}{
		{"abc-uploads", true},
		{strings.Repeat("a", 63), true},
		{strings.Repeat("a", 64), false},
		{"ab", false},
		{"-abc", false},
		{"abc-", false},
		{"abc--def", true},
		{"AbC", false},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			if got := validComposedName(tc.input); got != tc.want {
				t.Errorf("validComposedName(%q) = %v; want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestBuildPolicyJSON(t *testing.T) {
	t.Parallel()

	got := BuildPolicyJSON("kevin-u7")

	// Must contain both resource ARN patterns (bucket + object).
	if !strings.Contains(got, `"arn:aws:s3:::kevin-u7-*"`) {
		t.Errorf("policy missing bucket-scope ARN: %s", got)
	}
	if !strings.Contains(got, `"arn:aws:s3:::kevin-u7-*/*"`) {
		t.Errorf("policy missing object-scope ARN: %s", got)
	}
	// Must NOT grant CreateBucket / DeleteBucket — those go through root.
	if strings.Contains(got, "CreateBucket") {
		t.Errorf("policy unexpectedly grants CreateBucket: %s", got)
	}
	if strings.Contains(got, "DeleteBucket") {
		t.Errorf("policy unexpectedly grants DeleteBucket: %s", got)
	}
	// Must include ListAllMyBuckets so the SDK's `listBuckets()` works.
	if !strings.Contains(got, "ListAllMyBuckets") {
		t.Errorf("policy missing ListAllMyBuckets: %s", got)
	}
	// Must be valid JSON (Version field present).
	if !strings.Contains(got, `"Version":"2012-10-17"`) {
		t.Errorf("policy missing Version: %s", got)
	}
}

func TestSanitizeNameBody_NoTrailingHyphenWhenTruncated(t *testing.T) {
	t.Parallel()
	// 11-char input where the 12-char cap would land on a hyphen.
	in := "abcdefghij-kkkkkkk"
	got := sanitizeNameBody(in, 12)
	if strings.HasSuffix(got, "-") {
		t.Errorf("sanitizeNameBody(%q, 12) = %q; trailing hyphen leak", in, got)
	}
}

func TestRandomHexBytes(t *testing.T) {
	t.Parallel()
	a, err := randomHexBytes(32)
	if err != nil {
		t.Fatalf("randomHexBytes: %v", err)
	}
	b, err := randomHexBytes(32)
	if err != nil {
		t.Fatalf("randomHexBytes: %v", err)
	}
	if string(a) == string(b) {
		t.Errorf("two random samples collided: %s", a)
	}
	if len(a) != 64 {
		t.Errorf("len(a) = %d; want 64 hex chars from 32 bytes", len(a))
	}
}
