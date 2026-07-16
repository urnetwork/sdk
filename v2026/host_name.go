package sdk

import (
	"strings"
)

// the common multi-part public suffixes. a small embedded subset of the
// public suffix list (publicsuffix.org) covering the widely used country
// code second-level domains, so base names don't collapse past the
// registrable part ("example.co.uk" stays "example.co.uk", not "co.uk")
var multiPartPublicSuffixes = map[string]bool{
	// uk
	"co.uk": true, "org.uk": true, "me.uk": true, "net.uk": true,
	"ac.uk": true, "gov.uk": true, "ltd.uk": true, "plc.uk": true,
	"sch.uk": true, "nhs.uk": true,
	// au
	"com.au": true, "net.au": true, "org.au": true, "edu.au": true,
	"gov.au": true, "asn.au": true, "id.au": true,
	// br
	"com.br": true, "net.br": true, "org.br": true, "gov.br": true,
	"edu.br": true,
	// jp
	"co.jp": true, "ne.jp": true, "or.jp": true, "ac.jp": true,
	"ad.jp": true, "go.jp": true, "gr.jp": true, "ed.jp": true,
	"lg.jp": true,
	// cn
	"com.cn": true, "net.cn": true, "org.cn": true, "gov.cn": true,
	"edu.cn": true, "ac.cn": true,
	// in
	"co.in": true, "net.in": true, "org.in": true, "firm.in": true,
	"gen.in": true, "ind.in": true, "ac.in": true, "edu.in": true,
	"gov.in": true, "res.in": true,
	// nz
	"co.nz": true, "net.nz": true, "org.nz": true, "govt.nz": true,
	"ac.nz": true, "school.nz": true, "geek.nz": true, "gen.nz": true,
	"maori.nz": true,
	// za
	"co.za": true, "net.za": true, "org.za": true, "gov.za": true,
	"edu.za": true, "ac.za": true, "web.za": true,
	// kr
	"co.kr": true, "ne.kr": true, "or.kr": true, "re.kr": true,
	"pe.kr": true, "go.kr": true, "ac.kr": true,
	// mx
	"com.mx": true, "net.mx": true, "org.mx": true, "gob.mx": true,
	"edu.mx": true,
	// ar
	"com.ar": true, "net.ar": true, "org.ar": true, "gob.ar": true,
	"edu.ar": true,
	// tr
	"com.tr": true, "net.tr": true, "org.tr": true, "gov.tr": true,
	"edu.tr": true, "gen.tr": true, "web.tr": true, "k12.tr": true,
	// ru
	"com.ru": true, "net.ru": true, "org.ru": true, "pp.ru": true,
	// tw
	"com.tw": true, "net.tw": true, "org.tw": true, "edu.tw": true,
	"gov.tw": true, "idv.tw": true,
	// hk
	"com.hk": true, "net.hk": true, "org.hk": true, "edu.hk": true,
	"gov.hk": true, "idv.hk": true,
	// sg
	"com.sg": true, "net.sg": true, "org.sg": true, "edu.sg": true,
	"gov.sg": true, "per.sg": true,
	// il
	"co.il": true, "net.il": true, "org.il": true, "ac.il": true,
	"gov.il": true, "muni.il": true, "k12.il": true,
	// id
	"co.id": true, "net.id": true, "or.id": true, "web.id": true,
	"ac.id": true, "sch.id": true, "go.id": true, "mil.id": true,
	"biz.id": true, "my.id": true,
	// th
	"co.th": true, "net.th": true, "or.th": true, "ac.th": true,
	"go.th": true, "in.th": true,
	// ua
	"com.ua": true, "net.ua": true, "org.ua": true, "edu.ua": true,
	"gov.ua": true, "in.ua": true,
	// pl
	"com.pl": true, "net.pl": true, "org.pl": true, "edu.pl": true,
	"gov.pl": true, "info.pl": true, "biz.pl": true, "waw.pl": true,
	// vn
	"com.vn": true, "net.vn": true, "org.vn": true, "edu.vn": true,
	"gov.vn": true, "ac.vn": true, "biz.vn": true, "info.vn": true,
	// ph
	"com.ph": true, "net.ph": true, "org.ph": true, "edu.ph": true,
	"gov.ph": true,
	// my
	"com.my": true, "net.my": true, "org.my": true, "edu.my": true,
	"gov.my": true,
	// eg
	"com.eg": true, "net.eg": true, "org.eg": true, "edu.eg": true,
	"gov.eg": true,
	// sa
	"com.sa": true, "net.sa": true, "org.sa": true, "edu.sa": true,
	"gov.sa": true, "med.sa": true, "pub.sa": true, "sch.sa": true,
	// ae
	"co.ae": true, "net.ae": true, "org.ae": true, "ac.ae": true,
	"gov.ae": true, "sch.ae": true,
	// ke
	"co.ke": true, "or.ke": true, "ne.ke": true, "go.ke": true,
	"ac.ke": true, "sc.ke": true,
	// ng
	"com.ng": true, "net.ng": true, "org.ng": true, "edu.ng": true,
	"gov.ng": true,
	// co
	"com.co": true, "net.co": true, "org.co": true, "edu.co": true,
	"gov.co": true, "nom.co": true,
	// pe
	"com.pe": true, "net.pe": true, "org.pe": true, "gob.pe": true,
	"edu.pe": true,
	// ve
	"com.ve": true, "net.ve": true, "org.ve": true, "gob.ve": true,
	"edu.ve": true,
}

// HostBaseName returns the base name of a host: one label plus the
// public suffix ("cdn.a.example.com" -> "example.com",
// "cdn.a.example.co.uk" -> "example.co.uk"). hosts at or below the
// base return unchanged
func HostBaseName(host string) string {
	host = strings.TrimSuffix(host, ".")
	labels := strings.Split(host, ".")
	if len(labels) <= 2 {
		return host
	}
	lastTwo := strings.ToLower(strings.Join(labels[len(labels)-2:], "."))
	if multiPartPublicSuffixes[lastTwo] {
		return strings.Join(labels[len(labels)-3:], ".")
	}
	return strings.Join(labels[len(labels)-2:], ".")
}
