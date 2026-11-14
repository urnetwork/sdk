//go:build dnspresetcheck

// Checks every preset of the sdk's built-in dns catalog live (connect
// ADBLOCK3RD F3), from the host it runs on, before a catalog change merges:
//
//	go run -tags dnspresetcheck ./dnspresetcheck [-preset id] [-family 4|6|both]
//
// For each DoH url and, for a hostname url, each of its pins, of the families
// asked: an RFC 8484 GET over HTTP/2, offering ALPN as connect's DoH clients
// do (http2.ConfigureTransports puts h2 ahead of http/1.1), with the
// certificate verified against the url's host (an ip SAN for an ip literal);
// a normal name resolving; the canary answered as the catalog says; and the
// HTTPS record of the canary carrying no address hints that would let a
// browser connect past the block (B4). Then each preset resolves through
// connect's own DoH client, dialing the catalog's pins on the host path. A
// personal preset is checked with the provider's example id, which the
// provider does not know, so it answers as the catalog says it answers an
// unknown id. A check of a family the host has no route for fails; run from
// a dual-stack host before recording a preset's checked date.
//
// It reads the network and so never runs as a test. It exits non-zero when
// any check fails, and prints the date to record in each passing preset's
// checked field.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/http2"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
)

// A normal name every resolver answers.
const checkNormalName = "example.com"

// The HTTPS (SVCB) record type and its address hint keys (RFC 9460).
const (
	checkTypeHttps    = dnsmessage.Type(65)
	checkSvcIpv4Hint  = 4
	checkSvcIpv6Hint  = 6
	checkEdeOptionKey = 15
)

// The extended dns errors that mark a block (RFC 8914): Blocked, Censored,
// Filtered.
var checkBlockEdeCodes = []uint16{15, 16, 17}

// One answer to a query.
type checkResponse struct {
	rcode       dnsmessage.RCode
	addrs       []netip.Addr
	edeCodes    []uint16
	httpsHints  []netip.Addr
	negotiated  string
	certSubject string
}

func main() {
	presetFilter := flag.String("preset", "", "check only this preset id")
	family := flag.String("family", "both", "the families to check: 4, 6 or both")
	timeout := flag.Duration("timeout", 15*time.Second, "the bound on each query")
	flag.Parse()

	failures := 0
	passed := []string{}
	for _, target := range sdk.DnsPresetCheckTargets() {
		if *presetFilter != "" && target.PresetId != *presetFilter {
			continue
		}
		fmt.Printf("== %s (%s, %s), checked %s\n", target.PresetId, target.ProviderId, target.Group, target.Checked)
		presetFailures := 0
		for _, dohUrl := range dedupe(slices.Concat(target.DohUrlsIpv4, target.DohUrlsIpv6)) {
			u, err := url.Parse(dohUrl)
			if err != nil {
				fmt.Printf("  FAIL %s: %v\n", dohUrl, err)
				presetFailures += 1
				continue
			}
			for _, dialAddr := range dialAddrs(u, target) {
				if !familyWanted(dialAddr, *family) {
					continue
				}
				presetFailures += checkEndpoint(target, u, dialAddr, *timeout)
			}
		}
		presetFailures += checkThroughConnect(target, *family, *timeout)
		if presetFailures == 0 {
			passed = append(passed, target.PresetId)
		}
		failures += presetFailures
	}
	today := time.Now().UTC().Format("2006-01-02")
	fmt.Printf("\npassed on %s (record in checked): %s\n", today, strings.Join(passed, ", "))
	if 0 < failures {
		fmt.Printf("%d checks failed\n", failures)
		os.Exit(1)
	}
}

// The addresses an endpoint is dialed at: the ip of an ip-literal url, else
// the catalog's pins of the url's host.
func dialAddrs(u *url.URL, target *sdk.DnsPresetCheckTarget) []netip.Addr {
	if addr, err := netip.ParseAddr(u.Hostname()); err == nil {
		return []netip.Addr{addr}
	}
	addrs := []netip.Addr{}
	for _, pin := range slices.Concat(target.PinIpv4s, target.PinIpv6s) {
		if addr, err := netip.ParseAddr(pin); err == nil {
			addrs = append(addrs, addr)
		}
	}
	return addrs
}

func familyWanted(addr netip.Addr, family string) bool {
	switch family {
	case "4":
		return addr.Is4()
	case "6":
		return addr.Is6()
	default:
		return true
	}
}

func dedupe(values []string) []string {
	out := []string{}
	for _, value := range values {
		if !slices.Contains(out, value) {
			out = append(out, value)
		}
	}
	return out
}

// Checks one url at one dial address. Returns the number of failed checks.
func checkEndpoint(target *sdk.DnsPresetCheckTarget, u *url.URL, dialAddr netip.Addr, timeout time.Duration) int {
	label := fmt.Sprintf("%s at %s", u.String(), dialAddr)
	client := newCheckClient(u, dialAddr, timeout)
	defer client.CloseIdleConnections()
	failures := 0
	fail := func(format string, args ...any) {
		failures += 1
		fmt.Printf("  FAIL %s: %s\n", label, fmt.Sprintf(format, args...))
	}
	pass := func(format string, args ...any) {
		fmt.Printf("  ok   %s: %s\n", label, fmt.Sprintf(format, args...))
	}

	normal, err := checkQuery(client, u, checkNormalName, dnsmessage.TypeA, timeout)
	if err != nil {
		fail("%s A: %v", checkNormalName, err)
		return failures
	}
	if normal.negotiated != "h2" {
		fail("negotiated %q, not h2", normal.negotiated)
	} else {
		pass("tls verified for %s, h2", normal.certSubject)
	}
	switch target.UnknownIdAnswer {
	case "refused":
		if normal.rcode != dnsmessage.RCodeRefused {
			fail("the example id answered %s, the catalog expects REFUSED", normal.rcode)
		} else {
			pass("the example id is refused, as the catalog expects")
		}
		return failures
	}
	if normal.rcode != dnsmessage.RCodeSuccess || !resolvesReal(normal.addrs) {
		fail("%s A answered %s %v", checkNormalName, normal.rcode, normal.addrs)
	} else {
		pass("%s A resolved %v", checkNormalName, normal.addrs)
	}
	if target.CanaryName == "" {
		return failures
	}
	canary, err := checkQuery(client, u, target.CanaryName, dnsmessage.TypeA, timeout)
	if err != nil {
		fail("canary %s A: %v", target.CanaryName, err)
		return failures
	}
	switch target.BlockAnswer {
	case "null":
		if canary.rcode != dnsmessage.RCodeSuccess || len(canary.addrs) == 0 || resolvesReal(canary.addrs) {
			fail("canary %s answered %s %v, the catalog expects a sink", target.CanaryName, canary.rcode, canary.addrs)
		} else {
			pass("canary %s blocked with %v", target.CanaryName, canary.addrs)
		}
	case "nxdomain":
		if canary.rcode != dnsmessage.RCodeNameError || !blockEde(canary.edeCodes) {
			fail("canary %s answered %s ede %v, the catalog expects NXDOMAIN with a filtering extended error", target.CanaryName, canary.rcode, canary.edeCodes)
		} else {
			pass("canary %s blocked with NXDOMAIN ede %v", target.CanaryName, canary.edeCodes)
		}
	}
	https, err := checkQuery(client, u, target.CanaryName, checkTypeHttps, timeout)
	if err != nil {
		fail("canary %s HTTPS: %v", target.CanaryName, err)
	} else if resolvesReal(https.httpsHints) {
		fail("canary %s HTTPS record carries address hints %v", target.CanaryName, https.httpsHints)
	} else {
		pass("canary %s HTTPS record carries no address hints", target.CanaryName)
	}
	return failures
}

// An HTTP client that dials only dialAddr, verifies the certificate against
// the url's host, and offers ALPN as connect's DoH clients do.
func newCheckClient(u *url.URL, dialAddr netip.Addr, timeout time.Duration) *http.Client {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", net.JoinHostPort(dialAddr.String(), port))
		},
		TLSClientConfig:     &tls.Config{ServerName: u.Hostname()},
		TLSHandshakeTimeout: timeout,
	}
	if _, err := http2.ConfigureTransports(transport); err != nil {
		panic(err)
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

// One RFC 8484 GET.
func checkQuery(client *http.Client, u *url.URL, name string, qType dnsmessage.Type, timeout time.Duration) (*checkResponse, error) {
	query, err := buildQuery(name, qType)
	if err != nil {
		return nil, err
	}
	requestUrl := *u
	values := requestUrl.Query()
	values.Set("dns", base64.RawURLEncoding.EncodeToString(query))
	requestUrl.RawQuery = values.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestUrl.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/dns-message")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", response.StatusCode)
	}
	parsed, err := parseResponse(body)
	if err != nil {
		return nil, err
	}
	if response.TLS != nil {
		parsed.negotiated = response.TLS.NegotiatedProtocol
		if 0 < len(response.TLS.PeerCertificates) {
			parsed.certSubject = response.TLS.PeerCertificates[0].Subject.CommonName
		}
	}
	return parsed, nil
}

// A query with an EDNS OPT record, as connect's DoH queries carry.
func buildQuery(name string, qType dnsmessage.Type) ([]byte, error) {
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	dnsName, err := dnsmessage.NewName(name)
	if err != nil {
		return nil, err
	}
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{RecursionDesired: true})
	if err := builder.StartQuestions(); err != nil {
		return nil, err
	}
	if err := builder.Question(dnsmessage.Question{Name: dnsName, Type: qType, Class: dnsmessage.ClassINET}); err != nil {
		return nil, err
	}
	if err := builder.StartAdditionals(); err != nil {
		return nil, err
	}
	var optHeader dnsmessage.ResourceHeader
	if err := optHeader.SetEDNS0(1232, dnsmessage.RCodeSuccess, false); err != nil {
		return nil, err
	}
	if err := builder.OPTResource(optHeader, dnsmessage.OPTResource{}); err != nil {
		return nil, err
	}
	return builder.Finish()
}

func parseResponse(message []byte) (*checkResponse, error) {
	var parser dnsmessage.Parser
	header, err := parser.Start(message)
	if err != nil {
		return nil, err
	}
	response := &checkResponse{rcode: header.RCode}
	if err := parser.SkipAllQuestions(); err != nil {
		return nil, err
	}
	for {
		answerHeader, err := parser.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch answerHeader.Type {
		case dnsmessage.TypeA:
			resource, err := parser.AResource()
			if err != nil {
				return nil, err
			}
			response.addrs = append(response.addrs, netip.AddrFrom4(resource.A))
		case dnsmessage.TypeAAAA:
			resource, err := parser.AAAAResource()
			if err != nil {
				return nil, err
			}
			response.addrs = append(response.addrs, netip.AddrFrom16(resource.AAAA))
		case checkTypeHttps:
			resource, err := parser.UnknownResource()
			if err != nil {
				return nil, err
			}
			response.httpsHints = append(response.httpsHints, httpsHints(resource.Data)...)
		default:
			if err := parser.SkipAnswer(); err != nil {
				return nil, err
			}
		}
	}
	if err := parser.SkipAllAuthorities(); err != nil {
		return nil, err
	}
	for {
		additionalHeader, err := parser.AdditionalHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return nil, err
		}
		if additionalHeader.Type != dnsmessage.TypeOPT {
			if err := parser.SkipAdditional(); err != nil {
				return nil, err
			}
			continue
		}
		opt, err := parser.OPTResource()
		if err != nil {
			return nil, err
		}
		for _, option := range opt.Options {
			if option.Code == checkEdeOptionKey && 2 <= len(option.Data) {
				response.edeCodes = append(response.edeCodes, binary.BigEndian.Uint16(option.Data[:2]))
			}
		}
	}
	return response, nil
}

// The ipv4hint and ipv6hint addresses of an HTTPS record's data (RFC 9460):
// priority, an uncompressed target name, then the SvcParams.
func httpsHints(data []byte) []netip.Addr {
	reader := bytes.NewReader(data)
	var priority uint16
	if binary.Read(reader, binary.BigEndian, &priority) != nil {
		return nil
	}
	for {
		labelLength, err := reader.ReadByte()
		if err != nil {
			return nil
		}
		if labelLength == 0 {
			break
		}
		if _, err := reader.Seek(int64(labelLength), io.SeekCurrent); err != nil {
			return nil
		}
	}
	hints := []netip.Addr{}
	for 0 < reader.Len() {
		var key, length uint16
		if binary.Read(reader, binary.BigEndian, &key) != nil || binary.Read(reader, binary.BigEndian, &length) != nil {
			return hints
		}
		value := make([]byte, length)
		if _, err := io.ReadFull(reader, value); err != nil {
			return hints
		}
		switch key {
		case checkSvcIpv4Hint:
			for i := 0; i+4 <= len(value); i += 4 {
				hints = append(hints, netip.AddrFrom4([4]byte(value[i:i+4])))
			}
		case checkSvcIpv6Hint:
			for i := 0; i+16 <= len(value); i += 16 {
				hints = append(hints, netip.AddrFrom16([16]byte(value[i:i+16])))
			}
		}
	}
	return hints
}

// Whether any address is a real one, not a 0.0.0.0 or :: sink.
func resolvesReal(addrs []netip.Addr) bool {
	for _, addr := range addrs {
		if !addr.Unmap().IsUnspecified() {
			return true
		}
	}
	return false
}

func blockEde(edeCodes []uint16) bool {
	for _, edeCode := range edeCodes {
		if slices.Contains(checkBlockEdeCodes, edeCode) {
			return true
		}
	}
	return false
}

// Resolves a normal name through connect's own DoH client on the host path,
// dialing the catalog's pins, as Fast DNS on connect's host fallback would
// dial a custom list naming the preset. Returns the number of failed checks.
func checkThroughConnect(target *sdk.DnsPresetCheckTarget, family string, timeout time.Duration) int {
	settings := connect.DefaultDohSettings()
	settings.Log = connect.NewNoopLogger()
	switch family {
	case "4":
		settings.IpVersion = 4
	case "6":
		settings.IpVersion = 6
	}
	resolver := &connect.DnsResolverSettings{
		EnableLocalDoh:   true,
		LocalDohUrlsIpv4: target.DohUrlsIpv4,
		LocalDohUrlsIpv6: target.DohUrlsIpv6,
	}
	pins := slices.Concat(target.PinIpv4s, target.PinIpv6s)
	if 0 < len(pins) {
		resolver.DohServerHostAddrs = map[string][]string{}
		for _, dohUrl := range dedupe(slices.Concat(target.DohUrlsIpv4, target.DohUrlsIpv6)) {
			if u, err := url.Parse(dohUrl); err == nil {
				resolver.DohServerHostAddrs[strings.ToLower(u.Hostname())] = pins
			}
		}
	}
	settings.DnsResolverSettings = resolver
	cache := connect.NewDohCache(settings)
	defer cache.Close()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	addrs, _ := cache.QueryResult(ctx, "A", checkNormalName)
	if target.UnknownIdAnswer == "refused" {
		if 0 < len(addrs) {
			fmt.Printf("  FAIL connect's DoH client resolved %s with the example id; the catalog expects REFUSED\n", checkNormalName)
			return 1
		}
		fmt.Printf("  ok   connect's DoH client gets no answer for the example id, as the catalog expects\n")
		return 0
	}
	if !resolvesReal(addrs) {
		fmt.Printf("  FAIL connect's DoH client did not resolve %s through the preset: %v\n", checkNormalName, addrs)
		return 1
	}
	fmt.Printf("  ok   connect's DoH client resolved %s through the preset's pins: %v\n", checkNormalName, addrs)
	return 0
}
