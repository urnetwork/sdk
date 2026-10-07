// Exercises the exact server epoch wire shape through the actual sdk transport.
package sdk

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sync/atomic"
	"testing"
)

// The server emits no_id as a decimal string beside numeric schedule fields
// and the 0x-hex genesis hash. An id above the exact float64 range catches
// lossy intermediary decoding.
func TestApiSnEpochDecodesServerStringNoId(t *testing.T) {
	const bearerJwt = "synthetic-epoch-token"
	const genesisHash = "0xabababababababababababababababababababababababababababababababab"
	const response = `{"epoch":43,"start_block":10,"commit_deadline_block":20,"trails_deadline_block":30,"finalize_block":40,"t_epoch_blocks":50,"chain_id":9,"genesis_hash":"` + genesisHash + `","contract_address":"0x0000000000000000000000000000000000000007","settlement_vault_address":"0x0000000000000000000000000000000000000008","no_id":"9007199254740993","netuid":17,"rpc_url":"https://rpc.example"}`
	var requestCount atomic.Int64
	ctx, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		if r.Method != http.MethodGet || r.RequestURI != "/sn/epoch" {
			t.Errorf("epoch request = %s %s, want GET /sn/epoch", r.Method, r.RequestURI)
		}
		requireRequestBearer(t, r, bearerJwt)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, response)
	}))
	api.SetByJwt(bearerJwt)
	expected := SnEpochResult{
		Epoch: 43, StartBlock: 10, CommitDeadlineBlock: 20, TrailsDeadlineBlock: 30,
		FinalizeBlock: 40, TEpochBlocks: 50, ChainId: 9,
		GenesisHash:            genesisHash,
		ContractAddress:        "0x0000000000000000000000000000000000000007",
		SettlementVaultAddress: "0x0000000000000000000000000000000000000008",
		NoId:                   9007199254740993,
		Netuid:                 17,
		RpcUrl:                 "https://rpc.example",
	}
	for _, request := range []func() (*SnEpochResult, error){
		func() (*SnEpochResult, error) { return api.SnEpochSyncWithContext(ctx) },
		api.SnEpochSync,
	} {
		result, err := request()
		if err != nil || result == nil || *result != expected {
			t.Fatalf("server epoch result = %+v, %v, want %+v", result, err, expected)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil || string(fields["no_id"]) != "9007199254740993" {
			t.Fatalf("sdk no_id encoding = %s, %v, want unchanged exact numeric encoding", fields["no_id"], err)
		}
		if string(fields["genesis_hash"]) != `"`+genesisHash+`"` {
			t.Fatalf("sdk genesis_hash encoding = %s, want %q", fields["genesis_hash"], genesisHash)
		}
	}
	if requestCount.Load() != 2 || api.GetByJwt() != bearerJwt {
		t.Fatal("epoch binding did not preserve the wire requests and credential")
	}
}

// The public http binding preserves integer limits and compatibility defaults
// while refusing malformed supplied ids instead of returning a zero success.
func TestApiSnEpochNoIdWireBoundaries(t *testing.T) {
	responses := make(chan string, 1)
	ctx, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.RequestURI != "/sn/epoch" {
			t.Errorf("epoch request = %s %s, want GET /sn/epoch", r.Method, r.RequestURI)
		}
		w.Header().Set("Content-Type", "application/json")
		select {
		case response := <-responses:
			fmt.Fprint(w, response)
		case <-r.Context().Done():
		}
	}))
	cases := []struct {
		response string
		noId     int64
		wantErr  bool
	}{
		{response: `{"no_id":"0"}`, noId: 0},
		{response: `{"no_id":0}`, noId: 0},
		{response: `{"no_id":9007199254740993}`, noId: 9007199254740993},
		{response: `{"no_id":"9223372036854775807"}`, noId: math.MaxInt64},
		{response: `{"no_id":9223372036854775807}`, noId: math.MaxInt64},
		{response: `{"no_id":"-9223372036854775808"}`, noId: math.MinInt64},
		{response: `{"no_id":-9223372036854775808}`, noId: math.MinInt64},
		{response: `{}`, noId: 0},
		{response: `{"no_id":null}`, noId: 0},
		{response: `{"no_id":"9223372036854775808"}`, wantErr: true},
		{response: `{"no_id":9223372036854775808}`, wantErr: true},
		{response: `{"no_id":"-9223372036854775809"}`, wantErr: true},
		{response: `{"no_id":-9223372036854775809}`, wantErr: true},
		{response: `{"no_id":""}`, wantErr: true},
		{response: `{"no_id":"null"}`, wantErr: true},
		{response: `{"no_id":" 17 "}`, wantErr: true},
		{response: `{"no_id":"+17"}`, wantErr: true},
		{response: `{"no_id":"017"}`, wantErr: true},
		{response: `{"no_id":"1.5"}`, wantErr: true},
		{response: `{"no_id":1.5}`, wantErr: true},
		{response: `{"no_id":"1e2"}`, wantErr: true},
		{response: `{"no_id":1e2}`, wantErr: true},
		{response: `{"no_id":"0x11"}`, wantErr: true},
		{response: `{"no_id":true}`, wantErr: true},
		{response: `{"no_id":{}}`, wantErr: true},
		{response: `{"no_id":[]}`, wantErr: true},
	}
	for _, c := range cases {
		responses <- c.response
		result, err := api.SnEpochSyncWithContext(ctx)
		if c.wantErr {
			if err == nil || result != nil {
				t.Errorf("malformed epoch %s returned %+v, %v", c.response, result, err)
			}
		} else if err != nil || result == nil || result.NoId != c.noId {
			t.Errorf("epoch %s returned %+v, %v, want no_id %d", c.response, result, err, c.noId)
		}
	}
}

// Reusing a result retains the ordinary integer decoder's absent/null behavior;
// any malformed supplied occurrence leaves the original schedule intact.
func TestSnEpochNoIdDecodePreservesReceiver(t *testing.T) {
	original := SnEpochResult{Epoch: 42, NoId: 17, ContractAddress: "synthetic-coordinator"}
	cases := []struct {
		data    string
		noId    int64
		wantErr bool
	}{
		{data: `{"epoch":43}`, noId: 17},
		{data: `{"epoch":43,"no_id":null}`, noId: 17},
		{data: `{"epoch":43,"no_id":"19"}`, noId: 19},
		{data: `{"epoch":43,"no_id":19,"no_id":null}`, noId: 19},
		{data: `{"epoch":43,"no_id":"19","no_id":null}`, noId: 19},
		{data: `{"epoch":43,"no_id":"invalid"}`, wantErr: true},
		{data: `{"epoch":43,"no_id":"9223372036854775808"}`, wantErr: true},
		{data: `{"epoch":43,"no_id":"invalid","no_id":19}`, wantErr: true},
	}
	for _, c := range cases {
		result := original
		err := json.Unmarshal([]byte(c.data), &result)
		if c.wantErr {
			if err == nil || result != original {
				t.Errorf("malformed epoch %s changed receiver to %+v with error %v", c.data, result, err)
			}
		} else if err != nil || result.Epoch != 43 || result.NoId != c.noId || result.ContractAddress != original.ContractAddress {
			t.Errorf("epoch %s decoded to %+v, %v", c.data, result, err)
		}
	}
}
