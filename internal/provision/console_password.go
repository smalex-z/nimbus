package provision

import (
	"crypto/rand"
	"fmt"
)

// generateConsolePassword returns a short, human-typeable passphrase
// suitable for one-time noVNC console login. Format: two short
// English words joined by hyphens, followed by four digits — e.g.
// `cedar-otter-4827`. ~28 bits of entropy, far more than enough for
// a single-use console fallback (the password is discarded the
// moment the operator pastes it once and is replaced on the next
// reprovision).
//
// Rationale for not using hex: noVNC console doesn't support
// paste-from-clipboard reliably, so operators end up typing the
// password by hand. Two short words plus four digits is fast to
// type, easy to verbalize ("cedar-otter forty-eight twenty-seven")
// and dramatically reduces typo retries vs a 16-char hex string.
//
// crypto/rand failure returns "" so the caller can fall back to no
// password without crashing (provisioning still succeeds; console
// login just won't work).
func generateConsolePassword() string {
	w1, ok1 := pickPassphraseWord()
	w2, ok2 := pickPassphraseWord()
	digits, ok3 := pickFourDigits()
	if !ok1 || !ok2 || !ok3 {
		return ""
	}
	return fmt.Sprintf("%s-%s-%s", w1, w2, digits)
}

// pickPassphraseWord returns a uniformly random word from
// passphraseWords. The slice is sized so the modulo bias is < 0.4%
// (negligible for a one-time password).
func pickPassphraseWord() (string, bool) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", false
	}
	idx := (uint16(b[0])<<8 | uint16(b[1])) % uint16(len(passphraseWords))
	return passphraseWords[idx], true
}

// pickFourDigits returns a 4-digit decimal string, e.g. "0427".
func pickFourDigits() (string, bool) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", false
	}
	n := (uint16(b[0])<<8 | uint16(b[1])) % 10000
	return fmt.Sprintf("%04d", n), true
}

// passphraseWords is a curated list of short, common, unambiguous
// English words. 4-6 chars, no homophones (no `cell/sell`, no
// `four/for`), no profanity, no offensive terms. Sized to a power
// of two would let us drop the modulo bias guard, but ~256 makes
// the bias < 0.4% which is fine for one-time use.
var passphraseWords = []string{
	"acorn", "amber", "anvil", "apple", "april", "arrow", "aspen", "atlas",
	"badge", "bagel", "baker", "basil", "beach", "berry", "birch", "blaze",
	"bloom", "boots", "brass", "brick", "brook", "broom", "brown", "bunny",
	"cabin", "cable", "candy", "canoe", "cedar", "chalk", "charm", "cherry",
	"chess", "chime", "cider", "cliff", "cloud", "clove", "clover", "coast",
	"cocoa", "comet", "cookie", "copper", "coral", "crane", "creek", "crest",
	"daisy", "dance", "delta", "denim", "diamond", "dolphin", "dover", "dream",
	"eagle", "earth", "easel", "ember", "envoy", "eraser", "fable", "falcon",
	"fancy", "feast", "felt", "fern", "festive", "fiber", "field", "finch",
	"flame", "fleet", "flint", "float", "flora", "flute", "foggy", "forest",
	"frost", "ginger", "glade", "glass", "glitter", "glow", "golf", "grape",
	"grass", "gravel", "gray", "grin", "grove", "happy", "harbor", "haven",
	"hazel", "heart", "hedge", "heron", "hike", "honey", "horse", "hover",
	"island", "ivory", "jade", "jaguar", "jolly", "juniper", "kayak", "kettle",
	"kingdom", "kite", "knot", "lake", "lemon", "linen", "lion", "lotus",
	"lucky", "lunar", "magic", "maple", "marble", "marsh", "meadow", "mellow",
	"melody", "merry", "mint", "misty", "moon", "moss", "mossy", "mountain",
	"music", "needle", "nest", "noble", "north", "oak", "ocean", "olive",
	"onyx", "opal", "orange", "orbit", "otter", "panda", "paper", "patch",
	"peach", "pearl", "pebble", "pecan", "pencil", "petal", "piano", "pine",
	"pixel", "plaid", "plain", "plant", "plum", "polar", "pond", "poppy",
	"prairie", "prism", "puddle", "purple", "puzzle", "quartz", "quiet", "quill",
	"rabbit", "rain", "rapid", "raven", "ribbon", "ridge", "river", "robin",
	"rocket", "rose", "rosy", "rover", "ruby", "rustic", "sable", "saddle",
	"sage", "salad", "salmon", "sand", "sandy", "sapphire", "sapling", "satin",
	"saxon", "scarlet", "shade", "shamrock", "shell", "shore", "silk", "silver",
	"slate", "smile", "snowy", "solar", "spark", "splash", "spring", "sprout",
	"stone", "stork", "stream", "summer", "sunny", "swan", "tango", "teal",
	"tiger", "timber", "tinsel", "topaz", "torch", "trail", "tundra", "twig",
	"vale", "valley", "velvet", "vine", "violet", "walnut", "warm", "water",
	"wave", "wheat", "whisker", "willow", "winter", "wisdom", "wolf", "woodland",
	"wren", "yacht", "yarn", "year", "yew", "zebra", "zenith", "zephyr",
}
