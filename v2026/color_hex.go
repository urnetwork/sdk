package sdk

// Country/location color palette and the deterministic fallback mix.
//
// Kept in its own untagged file so the ios_extension build (the packet tunnel
// extension's SDK slice, which excludes the view controllers) exports
// GetColorHex too: the extension writes widget snapshots that carry each
// provider's country color, and the widget process links no SDK at all.

import (
	"crypto/md5"
	"fmt"
	"hash/fnv"
	"math"
	"net/netip"
	"sort"
	"strings"
)

func GetColorHex(code string) string {
	if color, exists := countryCodeColorHexes[code]; exists {
		return color
	}

	/**
	 * Fallback if color hex isn't found, generate a new one by mixing two colors together
	 */
	keys := make([]string, 0, len(countryCodeColorHexes))
	for k := range countryCodeColorHexes {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	index1 := getHashIndex(code, len(keys))
	index2 := getHashIndex(code+"salt", len(keys))

	color1 := countryCodeColorHexes[keys[index1]]
	color2 := countryCodeColorHexes[keys[index2]]

	return mixColors(color1, color2)
}

// GetExtenderColorHex is the one color of one extender address (EXTENDER.md
// K3), computed here so every app draws the same ring for the same extender:
// FNV-1a 32 over the canonical ip string, hue the hash modulo 360, saturation
// 70 percent, lightness 55 percent, as six hex digits with no leading `#`.
//
// The canonical form is what netip prints -- lower case, the shortest v6 form,
// a v4-mapped v6 address unmapped to its v4 form -- so the same address
// written two ways is one color. An input that does not parse is hashed as the
// trimmed text it is, which keeps the answer stable rather than empty.
//
// Fixed saturation and lightness are what make every hue legible on both the
// light and the dark background and keep two adjacent rings distinguishable by
// hue alone.
func GetExtenderColorHex(ip string) string {
	name := strings.TrimSpace(ip)
	if parsedIp, err := netip.ParseAddr(name); err == nil {
		name = parsedIp.Unmap().String()
	}
	hash := fnv.New32a()
	hash.Write([]byte(name))
	return hslColorHex(float64(hash.Sum32()%360), 0.70, 0.55)
}

// The standard hsl to rgb conversion, with hue in degrees and saturation and
// lightness in [0, 1]. Half-up rounding on each channel, so the value is a
// pure function of the three inputs on every platform.
func hslColorHex(hue float64, saturation float64, lightness float64) string {
	c := (1 - math.Abs(2*lightness-1)) * saturation
	h := hue / 60
	x := c * (1 - math.Abs(math.Mod(h, 2)-1))
	m := lightness - c/2
	var r, g, b float64
	switch {
	case h < 1:
		r, g, b = c, x, 0
	case h < 2:
		r, g, b = x, c, 0
	case h < 3:
		r, g, b = 0, c, x
	case h < 4:
		r, g, b = 0, x, c
	case h < 5:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	channel := func(v float64) int {
		return int(math.Round((v + m) * 255))
	}
	return rgbToHex(channel(r), channel(g), channel(b))
}

// to get a consistent index from the id
func getHashIndex(id string, mod int) int {
	hash := md5.Sum([]byte(id))
	return int(hash[0]) % mod
}

func mixColors(color1, color2 string) string {
	r1, g1, b1 := hexToRGB(color1)
	r2, g2, b2 := hexToRGB(color2)

	// Mix the colors by averaging their RGB components
	r := (r1 + r2) / 2
	g := (g1 + g2) / 2
	b := (b1 + b2) / 2

	return rgbToHex(r, g, b)
}

func hexToRGB(hex string) (int, int, int) {
	var r, g, b int
	fmt.Sscanf(hex, "%02x%02x%02x", &r, &g, &b)
	return r, g, b
}

func rgbToHex(r, g, b int) string {
	return fmt.Sprintf("%02x%02x%02x", r, g, b)
}

var countryCodeColorHexes = map[string]string{
	"is": "639A88",
	"ee": "78C0E0",
	"ca": "449DD1",
	"de": "663F46",
	"au": "F29E4C",
	"us": "BAC5B3",
	"gb": "F1789B",
	"jp": "CC3363",
	"ie": "7EE081",
	"fi": "F56E48",
	"nl": "F56E48",
	"se": "A4C4F4",
	"fr": "A864DC",
	"it": "F9F871",
	"dk": "D6E6F4",
	"no": "BCE5DC",
	"be": "9B4A91",
	"at": "FFCB68",
	"ch": "FFABA0",
	"nz": "008A64",
	"pt": "248C89",
	"es": "B41F43",
	"lv": "EEE8A9",
	"lt": "8179E0",
	"cz": "99E8CE",
	"si": "FF6C58",
	"sk": "87FB67",
	"pl": "D38B5D",
	"hu": "FF8484",
	"hr": "99B2DD",
	"ro": "C6362F",
	"bg": "A1CDF4",
	"gr": "C874D9",
	"cy": "E1BBC9",
	"mt": "FFC43D",
	"il": "A9E4EF",
	"za": "F2B79F",
	"ar": "8E8DBE",
	"br": "DCD6F7",
	"cl": "FA824C",
	"cr": "E07A5F",
	"uy": "7FDEFF",
	"jm": "7B886F",
	"tt": "0072BB",
	"gh": "1098F7",
	"ke": "F2EDEB",
	"ng": "64113F",
	"tn": "6B4D57",
	"sn": "596869",
	"na": "813405",
	"bw": "D45113",
	"mu": "60E1E0",
	"mg": "F25D72",
	"in": "F2E2D2",
	"kr": "C320D9",
	"tw": "E6EA23",
	"my": "3A1772",
	"ph": "B4CEB3",
	"id": "586189",
	"mn": "A6A57A",
	"ge": "679436",
	"am": "F2B5D4",
	"ua": "00F28D",
	"md": "7F675B",
	"me": "E5FFDE",
	"rs": "FF495C",
	"al": "E4B363",
}
