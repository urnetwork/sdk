/* A C PROGRAM THAT OPENS TWO DEVICES, FOUNDS A GROUP, JOINS IT, SENDS OCTETS AND READS THEM
 * BACK, THROUGH THE SHIPPING C ABI AND NOTHING ELSE.
 *
 * It exists because the boundary is the whole point of the binding: a Go test of exports_message.go
 * would call Go functions with Go types and would never once exercise a uint64_t handle, a
 * malloc'd char*, a buffer-out length negotiation or a C function pointer. Every line below is on
 * the C side of the cgo wall.
 *
 * WHAT IS REAL HERE AND WHAT IS THE HARNESS. The server is real: peer.Peer dispatching frames,
 * api.Handler running the §5.1 pipeline, store.MemoryStore holding rows, wired exactly as
 * sdk/cp3b's world_test.go wires it. The urnet_message_loopback_* functions are the harness and
 * they are NOT in the shipping library -- they are behind a Go build tag and run.sh proves the
 * shipping header has none of them.
 *
 * THERE USED TO BE THREE MORE OF THEM AND THEY WERE THE SEND VERBS. A reply, a reaction and a
 * tombstone had to be produced by the HARNESS, because no shipping export sealed one; this file's
 * envelope steps could therefore prove the read projection and could not prove that a C caller can
 * take part. Those three are gone. urnet_message_group_send_reply, _react, _unreact and _delete
 * SHIP, and the four steps below drive those -- so what they measure now is the whole verb, from a
 * C caller's call to the far device's row.
 *
 * AND THE ROLE MODEL'S SURFACE IS DRIVEN FROM HERE TOO: the roster, this device's role, and the two
 * policy verbs, with the owner's promotion of B landing on B's roster through a real receive, and
 * the member's attempt on the owner REFUSED by kind with nothing moved anywhere -- the two arms of
 * MASTER section 11 as a C caller sees them.
 *
 * WHY A HARNESS IS NEEDED AT ALL, AND IT IS NO LONGER "NO SHIPPING EXPORT PRODUCES A CLIENT".
 * urnet_message_client_new does, and the step near the end of this file builds one. What it cannot
 * do is reach anything: there is no operator here to dial and no credential to dial one with, so
 * the CONVERSATION below runs over an in-process server and the platform client is exercised for
 * its shape alone. EVERYTHING ELSE -- every store, every device, every group, every octet --
 * goes through the abi that ships.
 *
 * SPDX-License-Identifier: MPL-2.0 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <stdbool.h>
#include <windows.h>

#include "urnetwork_message.h"

/* from urnetwork_sdk.h, which is 122 KB of the VPN surface this test does not use. Declared here
 * rather than included so that a reader can see the whole of what this test depends on. */
extern void urnet_free_string(char* s);
extern bool urnet_release(uint64_t handle);
extern int64_t urnet_live_handle_count(void);

/* the harness. NOT IN THE SHIPPING LIBRARY -- see the file header. */
extern uint64_t urnet_message_loopback_world_new(char** out_error);
/* and the one thing that is NOT real: three entries built in Go. nothing in this tree can seal a
 * malformed body or an unknown kind from outside urmessage (msgrepo ledger item 235), so the WALK
 * that makes a gap is held by urmessage's own suite and what is held HERE is that a gap crosses the
 * boundary as something a C caller can tell from a message. */
extern uint64_t urnet_message_loopback_gap_list(void);
extern char* urnet_message_loopback_world_server_id(uint64_t self);
extern uint64_t urnet_message_loopback_world_client(uint64_t self);
extern uint64_t urnet_message_loopback_world_unrouted_client(uint64_t self);
extern void urnet_message_loopback_world_omit_highest(uint64_t self, bool on);
extern void urnet_message_loopback_world_close(uint64_t self);

/* ── assertions ──────────────────────────────────────────────────────────────────────────── */

static int checks = 0;
static int failures = 0;
static int steps = 0;

#define CHECK(cond, ...)                                                    \
  do {                                                                      \
    checks += 1;                                                            \
    if (!(cond)) {                                                          \
      failures += 1;                                                        \
      fprintf(stderr, "  FAIL %s:%d: ", __FILE__, __LINE__);                \
      fprintf(stderr, __VA_ARGS__);                                         \
      fprintf(stderr, "\n");                                                \
    }                                                                       \
  } while (0)

/* REQUIRE is CHECK plus "everything after this is meaningless": a run that lost its group cannot
 * go on to say anything about a message. It returns from main rather than continuing to print
 * passes that mean nothing.
 *
 * THE CONDITION IS EVALUATED EXACTLY ONCE, into urnet_ok_. It used to expand to
 * `CHECK(cond); if (!(cond))`, which ran REQUIRE(party_open(...)) twice and stood up every device
 * twice -- and the handle-count assertion at the end of this file is what caught it, at 18
 * handles leaked, which is exactly three parties' worth of the second copy. */
#define REQUIRE(cond, ...)                                                  \
  do {                                                                      \
    bool urnet_ok_ = (cond);                                                \
    CHECK(urnet_ok_, __VA_ARGS__);                                          \
    if (!urnet_ok_) {                                                       \
      report();                                                             \
      return 1;                                                             \
    }                                                                       \
  } while (0)

static void report(void);

/* Every step prints the live handle count, so that a leak is located at the step that made it
 * rather than only totalled at the end. */
static void step(const char* what) {
  steps += 1;
  printf("[%d] (%lld live) %s\n", steps, (long long)urnet_live_handle_count(), what);
  fflush(stdout);
}

/* err_of prints and frees an out_error, and answers a stable string for the message. */
static void show_error(const char* what, char* err) {
  if (err != NULL) {
    fprintf(stderr, "  %s: %s\n", what, err);
    urnet_free_string(err);
  } else {
    fprintf(stderr, "  %s: (no error text)\n", what);
  }
}

/* ── reading one string field out of the info json ───────────────────────────────────────────
 *
 * Deliberately a `"key":"` search and a copy up to the next quote, rather than a json parser:
 * this program links nothing but the library under test, and the fields it reads are hex written
 * by encoding/json a few lines away. It answers false when the key is absent or when the value is
 * not a string, which is the only failure a caller here can have. */
static bool json_string_field(const char* json, const char* key, char* out, size_t cap) {
  char needle[64];
  snprintf(needle, sizeof(needle), "\"%s\":\"", key);
  const char* at = (json == NULL) ? NULL : strstr(json, needle);
  if (at == NULL) {
    return false;
  }
  at += strlen(needle);
  const char* end = strchr(at, '"');
  if (end == NULL || (size_t)(end - at) >= cap) {
    return false;
  }
  memcpy(out, at, (size_t)(end - at));
  out[end - at] = '\0';
  return true;
}

/* is this 64 lower-case hex characters, not all of them zero? BOTH HALVES MATTER: a field that is
 * present and empty, and one that is thirty two zero octets, are the two shapes a message_id takes
 * when nothing derived it -- and a strstr for "message_id" alone would accept either. */
static bool is_message_id(const char* value) {
  size_t n = strlen(value);
  if (n != 64) {
    return false;
  }
  bool any = false;
  for (size_t at = 0; at < n; at += 1) {
    char c = value[at];
    if (!((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'))) {
      return false;
    }
    if (c != '0') {
      any = true;
    }
  }
  return any;
}

/* ── a message_id, from the hex it arrives as to the octets the send verbs take ──────────────
 *
 * THIS FUNCTION IS PART OF WHAT THE TEST PROVES AND NOT A CONVENIENCE. The four verbs that name
 * another message take a message_id as COUNTED OCTETS, because that is exports_message.go's rule
 * for every binary value going in -- group_id and key_package both -- and a message_id comes BACK
 * as 64 lower-case hex characters, in urnet_message_list_info's message_id field, because metadata
 * crosses as json. So a real caller writes exactly this loop once, and if the two ends did not
 * agree on the encoding every assertion below would fail with ErrNoSuchMessage.
 *
 * ID_OCTETS IS 32 AND IS SPELLED ONCE. A caller that passed the wrong length is refused by name
 * before anything is sealed, which is a step of its own further down. */
#define ID_OCTETS 32

static int hex_nibble(char c) {
  if (c >= '0' && c <= '9') {
    return c - '0';
  }
  if (c >= 'a' && c <= 'f') {
    return c - 'a' + 10;
  }
  if (c >= 'A' && c <= 'F') {
    return c - 'A' + 10;
  }
  return -1;
}

static bool hex_to_id(const char* hex, uint8_t* out) {
  if (hex == NULL || strlen(hex) != (size_t)(ID_OCTETS * 2)) {
    return false;
  }
  for (size_t at = 0; at < (size_t)ID_OCTETS; at += 1) {
    int high = hex_nibble(hex[at * 2]);
    int low = hex_nibble(hex[at * 2 + 1]);
    if (high < 0 || low < 0) {
      return false;
    }
    out[at] = (uint8_t)((high << 4) | low);
  }
  return true;
}

/* how many records a group has SUBMITTED, off urnet_message_group_stats. It is the number a
 * refusal step holds: a verb that refuses AFTER sealing has spent a stream index and an mls
 * generation, and answers NULL exactly as one that refused before the seal does. -1 means the
 * counter could not be read at all, which is a failure of the step rather than a value. */
static int submitted_by(uint64_t group) {
  char* stats = urnet_message_group_stats(group);
  if (stats == NULL) {
    return -1;
  }
  const char* at = strstr(stats, "\"submitted\":");
  int submitted = (at == NULL) ? -1 : (int)strtol(at + strlen("\"submitted\":"), NULL, 10);
  urnet_free_string(stats);
  return submitted;
}

/* any one counter off urnet_message_group_stats, by its json key; -1 when it cannot be read. The
 * roles step reads commit_refused_own with it, which is the number a REFUSED verb leaves behind. */
static int counter_of(uint64_t group, const char* key) {
  char needle[64];
  snprintf(needle, sizeof(needle), "\"%s\":", key);
  char* stats = urnet_message_group_stats(group);
  if (stats == NULL) {
    return -1;
  }
  const char* at = strstr(stats, needle);
  int value = (at == NULL) ? -1 : (int)strtol(at + strlen(needle), NULL, 10);
  urnet_free_string(stats);
  return value;
}

/* one roster row's role and identity, found by whether it is this device's own. It answers false
 * when no row with that mine value exists, when a row lacks a field, or when two rows claim it --
 * a roster that marks two leaves as one device's own is not a roster. */
static bool roster_row(uint64_t roster, bool mine, char* role, size_t role_cap,
                       char* identity, size_t identity_cap) {
  int32_t count = urnet_message_member_list_count(roster);
  int found = 0;
  for (int32_t at = 0; at < count; at += 1) {
    char* info = urnet_message_member_list_info(roster, at);
    if (info == NULL) {
      return false;
    }
    bool is_mine = strstr(info, "\"mine\":true") != NULL;
    if (is_mine == mine) {
      found += 1;
      if (!json_string_field(info, "role", role, role_cap) ||
          !json_string_field(info, "identity_pub", identity, identity_cap)) {
        urnet_free_string(info);
        return false;
      }
    }
    urnet_free_string(info);
  }
  return found == 1;
}

/* ── a temp directory per store ──────────────────────────────────────────────────────────── */

static char temp_root[MAX_PATH];
static int temp_n = 0;

/* into a buffer the CALLER owns. It used to answer a pointer into a static, which made two calls
 * one directory -- and the durable state store's single-writer exclusion is what caught it, which
 * is that store working exactly as its document says it does. */
static bool temp_dir(char* path, size_t cap) {
  temp_n += 1;
  snprintf(path, cap, "%surnet_ctest_%lu_%d", temp_root,
           (unsigned long)GetCurrentProcessId(), temp_n);
  if (!CreateDirectoryA(path, NULL)) {
    fprintf(stderr, "  could not create %s (%lu)\n", path, GetLastError());
    return false;
  }
  return true;
}

/* ── the body. IT IS NOT A C STRING, ON PURPOSE. ─────────────────────────────────────────────
 *
 * 0x00 at offsets 12 and 19 would truncate a char*-carried body to 12 octets with no error raised
 * anywhere, and the two multi-byte sequences are there so that what crosses is not seven-bit. A
 * byte-identical round trip is the measurement that says neither happened.
 *
 * IT USED TO CARRY 0xFF 0xFE AND THE ILL-FORMED 0xC3 0x28, AND IT NO LONGER CAN, WHICH IS RECORDED
 * HERE RATHER THAN QUIETLY REPAIRED. Since the content envelope landed (urmessage/kind.go, the
 * 2026-09-17 ruling) a TEXT tail is checked for valid UTF-8 BEFORE it is sealed, so
 * urnet_message_group_send refuses those octets -- "the 21 octet text tail is not valid utf8" --
 * and THIS FILE WAS ALREADY RED FOR THAT REASON AT sdk bd4672d, at the first send, before any of
 * the content-envelope steps below existed. Nothing runs it but `make ctest`, so nothing said so.
 *
 * The half those octets defended -- that a json-carried body would come back as U+FFFD -- is
 * measured in exports_message_test.go's TestTheRejectedBodyEncodingsWouldHaveChangedTheseOctets,
 * which has nothing to seal and can therefore still hold them. The half that is still measurable
 * HERE, across a real seal and a real receive, is the NUL, and it is the one that decides char*
 * against counted octets. */
static const unsigned char kBody[] = {
  'h', 'e', 'l', 'l', 'o', ' ', 'f', 'r', 'o', 'm', ' ', 'C',
  0x00, 0xC3, 0xA9, 0xE2, 0x9C, 0x93, '\n', 0x00, 'z'
};
static const int32_t kBodyLen = (int32_t)sizeof(kBody);

/* B's second plain message. 0xC2 0xA1 for the same reason as above: it was 0x80, which is a
 * continuation octet on its own, is not valid UTF-8, and is refused at the seal. */
static const unsigned char kReply[] = { 'r', 'e', 'p', 'l', 'y', 0x00, 0xC2, 0xA1, '!' };
static const int32_t kReplyLen = (int32_t)sizeof(kReply);

/* a REPLY's text is a text tail and IS checked for valid utf-8 on the way in and on the way out,
 * unlike a plain body, so this one is text. (kReply above is NOT a reply -- it is B's second plain
 * message and it is named for the conversation and not for the kind.) */
static const unsigned char kReplyText[] = "answering the first line";
static const int32_t kReplyTextLen = (int32_t)sizeof(kReplyText) - 1;

/* written as octets rather than pasted, so that this file stays ascii and so that what is compared
 * is the four octets and not what an editor decided they were. */
static const char kThumbsUp[] = "\xF0\x9F\x91\x8D";  /* U+1F44D */
static const char kDirectHit[] = "\xF0\x9F\x8E\xAF"; /* U+1F3AF */

/* ── the connect-attempt callback, which is the listener convention ──────────────────────── */

typedef struct {
  volatile LONG calls;
  int32_t last_attempt;
} attempt_counter;

static void on_connect_attempt(void* user_data, int32_t attempt, int64_t elapsed_ms,
                               int64_t backoff_ms, const char* err) {
  attempt_counter* counter = (attempt_counter*)user_data;
  if (counter == NULL) {
    return;
  }
  InterlockedIncrement(&counter->calls);
  counter->last_attempt = attempt;
  printf("      reconnecting: attempt %d, %lldms elapsed, %lldms backoff, %s\n",
         (int)attempt, (long long)elapsed_ms, (long long)backoff_ms,
         err != NULL ? err : "(no error)");
  fflush(stdout);
}

/* ── one device, built entirely through the shipping abi ─────────────────────────────────── */

typedef struct {
  const char* name;
  uint64_t client;
  uint64_t transport;
  uint64_t stream_store;
  uint64_t reserver;
  uint64_t state_store;
  uint64_t device;
} party;

static bool party_open(party* self, const char* name, uint64_t client, const char* server_id,
                       attempt_counter* counter, int64_t budget_ms, int64_t attempt_ms) {
  char* err = NULL;
  self->name = name;
  self->client = client;
  self->transport = urnet_message_transport_new(client, server_id, URNET_MESSAGE_PROTOCOL_VERSION, 30000, &err);
  if (self->transport == 0) {
    show_error("transport_new", err);
    return false;
  }
  char stream[MAX_PATH + 64];
  char state[MAX_PATH + 64];
  if (!temp_dir(stream, sizeof(stream)) || !temp_dir(state, sizeof(state))) {
    return false;
  }
  self->stream_store = urnet_message_stream_store_open(stream, &err);
  if (self->stream_store == 0) {
    show_error("stream_store_open", err);
    return false;
  }
  self->reserver = urnet_message_stream_index_reserver_new(self->stream_store);
  if (self->reserver == 0) {
    fprintf(stderr, "  reserver_new answered 0\n");
    return false;
  }
  /* the DURABLE state store, not the in-memory default: a real caller persists, and the nil
   * interface trap in urnet_message_device_new is only exercised by passing a real one. */
  self->state_store = urnet_message_durable_state_store_open(state, &err);
  if (self->state_store == 0) {
    show_error("durable_state_store_open", err);
    return false;
  }
  self->device = urnet_message_device_new(self->transport, self->reserver, self->state_store,
                                          budget_ms, attempt_ms, on_connect_attempt, counter, &err);
  if (self->device == 0) {
    show_error("device_new", err);
    return false;
  }
  return true;
}

/* party_close is stop-then-release, in that order, for every handle this party owns. It is what
 * the handle-count measurement at the end is measuring. */
static void party_close(party* self) {
  char* err = NULL;
  if (self->device != 0) {
    if (!urnet_message_device_close(self->device, &err)) {
      show_error("device_close", err);
    }
    CHECK(urnet_release(self->device), "%s: releasing the device answered false", self->name);
  }
  if (self->state_store != 0) {
    err = NULL;
    if (!urnet_message_durable_state_store_close(self->state_store, &err)) {
      show_error("state_store_close", err);
    }
    CHECK(urnet_release(self->state_store), "%s: releasing the state store answered false", self->name);
  }
  if (self->reserver != 0) {
    CHECK(urnet_release(self->reserver), "%s: releasing the reserver answered false", self->name);
  }
  if (self->stream_store != 0) {
    err = NULL;
    if (!urnet_message_stream_store_close(self->stream_store, &err)) {
      show_error("stream_store_close", err);
    }
    CHECK(urnet_release(self->stream_store), "%s: releasing the stream store answered false", self->name);
  }
  if (self->transport != 0) {
    urnet_message_transport_close(self->transport);
    CHECK(urnet_release(self->transport), "%s: releasing the transport answered false", self->name);
  }
  if (self->client != 0) {
    CHECK(urnet_release(self->client), "%s: releasing the client answered false", self->name);
  }
  memset(self, 0, sizeof(*self));
}

/* ── the cancellation thread ─────────────────────────────────────────────────────────────── */

typedef struct {
  uint64_t ctx;
  DWORD after_ms;
} canceller;

static DWORD WINAPI cancel_after(LPVOID arg) {
  canceller* self = (canceller*)arg;
  Sleep(self->after_ms);
  urnet_message_context_cancel(self->ctx);
  return 0;
}

/* ── the run ─────────────────────────────────────────────────────────────────────────────── */

static int64_t baseline_handles = 0;

static void report(void) {
  printf("\n=== %d STEPS, %d ASSERTIONS, %d FAILED ===\n", steps, checks, failures);
  fflush(stdout);
}

int main(void) {
  char* err = NULL;

  if (GetTempPathA(sizeof(temp_root), temp_root) == 0) {
    fprintf(stderr, "GetTempPathA failed\n");
    return 1;
  }

  step("the handle registry, before anything");
  baseline_handles = urnet_live_handle_count();
  printf("      %lld live handles\n", (long long)baseline_handles);

  step("a message server, in process, real");
  uint64_t world = urnet_message_loopback_world_new(&err);
  REQUIRE(world != 0, "the loopback world would not start");
  char* server_id = urnet_message_loopback_world_server_id(world);
  REQUIRE(server_id != NULL, "the world named no server");
  printf("      server %s\n", server_id);

  step("two devices, each with its own durable stream store and durable mls state store");
  attempt_counter counter_a = {0, 0};
  attempt_counter counter_b = {0, 0};
  party a = {0};
  party b = {0};
  REQUIRE(party_open(&a, "A", urnet_message_loopback_world_client(world), server_id, &counter_a, 0, 0),
          "A would not open");
  REQUIRE(party_open(&b, "B", urnet_message_loopback_world_client(world), server_id, &counter_b, 0, 0),
          "B would not open");
  urnet_free_string(server_id);
  server_id = NULL;

  step("a cancellable context, which every blocking call below rides on");
  uint64_t ctx = urnet_message_context_new();
  REQUIRE(ctx != 0, "context_new answered 0");

  step("Hello, on both");
  err = NULL;
  REQUIRE(urnet_message_device_connect(a.device, ctx, &err), "A could not connect");
  err = NULL;
  REQUIRE(urnet_message_device_connect(b.device, ctx, &err), "B could not connect");
  CHECK(counter_a.calls == 0, "A connected first time and the attempt callback fired %ld times",
        (long)counter_a.calls);

  step("B's key package, through the buffer-out pattern");
  int32_t kp_len = 0;
  bool sized = urnet_message_device_key_package(b.device, NULL, &kp_len, &err);
  CHECK(!sized, "a sizing call with out == NULL answered true, which would mean it copied somewhere");
  REQUIRE(kp_len > 0, "B's key package sized to %d octets", (int)kp_len);
  printf("      %d octets\n", (int)kp_len);
  uint8_t* key_package = (uint8_t*)malloc((size_t)kp_len);
  REQUIRE(key_package != NULL, "out of memory");
  int32_t kp_cap = kp_len;
  err = NULL;
  REQUIRE(urnet_message_device_key_package(b.device, key_package, &kp_cap, &err),
          "B's key package would not fill a buffer of exactly its own size");
  CHECK(kp_cap == kp_len, "the fill call reported %d octets and the sizing call said %d",
        (int)kp_cap, (int)kp_len);
  /* and one octet short must REFUSE rather than write past the end */
  int32_t kp_short = kp_len - 1;
  err = NULL;
  CHECK(!urnet_message_device_key_package(b.device, key_package, &kp_short, &err),
        "a buffer one octet short was accepted");
  CHECK(kp_short == kp_len, "the refused call reported %d octets, not the needed %d",
        (int)kp_short, (int)kp_len);

  step("A founds a group and adds B, which is the commit that opens epoch 1");
  uint8_t group_id[32];
  for (int at = 0; at < 32; at += 1) {
    group_id[at] = (uint8_t)(0xA0 + at);
  }
  err = NULL;
  uint64_t group_a = urnet_message_device_create_group(a.device, ctx, group_id, 32, &err);
  if (group_a == 0) {
    show_error("create_group", err);
  }
  REQUIRE(group_a != 0, "A could not found the group");
  err = NULL;
  uint64_t invite = urnet_message_group_add_member(group_a, key_package, kp_len, &err);
  if (invite == 0) {
    show_error("add_member", err);
  }
  REQUIRE(invite != 0, "A could not add B");
  free(key_package);
  key_package = NULL;

  step("the invite crosses as octets, and is parsed back on the other side");
  int32_t invite_len = 0;
  err = NULL;
  urnet_message_invite_encode(invite, NULL, &invite_len, &err);
  REQUIRE(invite_len > 0, "the invite sized to %d octets", (int)invite_len);
  uint8_t* encoded = (uint8_t*)malloc((size_t)invite_len);
  REQUIRE(encoded != NULL, "out of memory");
  int32_t invite_cap = invite_len;
  err = NULL;
  REQUIRE(urnet_message_invite_encode(invite, encoded, &invite_cap, &err),
          "the invite would not encode");
  printf("      %d octets of key material\n", (int)invite_len);
  err = NULL;
  uint64_t carried = urnet_message_parse_invite(encoded, invite_len, &err);
  if (carried == 0) {
    show_error("parse_invite", err);
  }
  REQUIRE(carried != 0, "the encoded invite would not parse back");
  /* A BLOB FROM ANOTHER BUILD IS REFUSED RATHER THAN READ AS THIS ONE: the first two octets are
   * the invite version, and ParseInvite refuses a version it did not write. */
  encoded[0] ^= 0xFF;
  err = NULL;
  uint64_t wrong_version = urnet_message_parse_invite(encoded, invite_len, &err);
  CHECK(wrong_version == 0, "an invite at a version this build does not write was parsed anyway");
  if (wrong_version != 0) {
    urnet_release(wrong_version);
  }
  CHECK(err != NULL, "the refused invite came back with no error text");
  if (err != NULL) {
    urnet_free_string(err);
    err = NULL;
  }
  encoded[0] ^= 0xFF;
  /* AND A DAMAGED ONE IS REFUSED AT THE PASTE, which this comment used to say it was not: an
   * invite was length-prefixed fields and nothing else, so one bit flipped inside group_handle_key
   * PARSED, JOINED as the intended recipient, and then never received a message. It now ends with
   * a checksum of everything before it. One bit, 40 octets from the end -- inside
   * group_handle_key, the field the review corrupted -- and it must not parse. */
  encoded[invite_len - 40] ^= 0x01;
  err = NULL;
  uint64_t damaged = urnet_message_parse_invite(encoded, invite_len, &err);
  CHECK(damaged == 0, "an invite with one bit flipped inside group_handle_key was parsed");
  if (damaged != 0) {
    urnet_release(damaged);
  }
  CHECK(err != NULL && strstr(err, "damaged") != NULL,
        "the damaged invite was refused without saying it is damaged: %s", err != NULL ? err : "(no error text)");
  if (err != NULL) {
    urnet_free_string(err);
    err = NULL;
  }
  encoded[invite_len - 40] ^= 0x01;
  err = NULL;
  uint64_t restored_invite = urnet_message_parse_invite(encoded, invite_len, &err);
  CHECK(restored_invite != 0, "the same invite with the bit put back did not parse, so the refusal above was not about the bit");
  if (restored_invite != 0) {
    urnet_release(restored_invite);
  }
  if (err != NULL) {
    urnet_free_string(err);
    err = NULL;
  }
  free(encoded);
  encoded = NULL;

  step("A opens the group on the server");
  err = NULL;
  if (!urnet_message_group_open(group_a, ctx, &err)) {
    show_error("group_open", err);
    REQUIRE(false, "A could not open the group");
  }
  CHECK(urnet_message_group_is_open(group_a), "A's group says it is not open after open");
  CHECK(urnet_message_group_epoch(group_a) == 1, "A's group is at epoch %llu, want 1",
        (unsigned long long)urnet_message_group_epoch(group_a));
  int32_t id_len = 0;
  urnet_message_group_id(group_a, NULL, &id_len);
  CHECK(id_len == 32, "the group id is %d octets, want 32", (int)id_len);
  uint8_t id_out[32];
  int32_t id_cap = 32;
  CHECK(urnet_message_group_id(group_a, id_out, &id_cap), "the group id would not copy out");
  CHECK(memcmp(id_out, group_id, 32) == 0, "the group id came back different from the one founded");

  step("B joins");
  err = NULL;
  uint64_t group_b = urnet_message_device_join(b.device, ctx, carried, &err);
  if (group_b == 0) {
    show_error("join", err);
  }
  REQUIRE(group_b != 0, "B could not join");
  CHECK(urnet_release(carried), "releasing the parsed invite answered false");
  CHECK(urnet_release(invite), "releasing A's invite answered false");
  invite = 0;
  carried = 0;

  step("A sends 21 octets that are not a C string: two NULs and two multi-byte sequences");
  err = NULL;
  char* sent_info = urnet_message_group_send(group_a, ctx, kBody, kBodyLen, &err);
  if (sent_info == NULL) {
    show_error("send", err);
  }
  REQUIRE(sent_info != NULL, "A's send failed");
  printf("      %s\n", sent_info);
  CHECK(strstr(sent_info, "\"mine\":true") != NULL, "A's own message did not say mine:true");
  CHECK(strstr(sent_info, "\"body_len\":21") != NULL, "A's send reported a body_len that is not 21");
  /* MASTER 8.4.5's message_id, WHICH THE SENDER HAS BEFORE ANYBODY ELSE DOES. It is derived from
   * the record alone, so it is available at the submit rather than at the answer -- which is what
   * lets a reply name its parent optimistically. Kept here and compared against B's below: an id
   * only one side can compute is a number, not a name. */
  char sent_message_id[128] = { 0 };
  CHECK(json_string_field(sent_info, "message_id", sent_message_id, sizeof(sent_message_id)),
        "A's send reported no message_id at all");
  CHECK(is_message_id(sent_message_id),
        "A's message_id is %s, which is not 32 non-zero octets of hex", sent_message_id);
  /* AND THE SAME NAME AS OCTETS, WHICH IS WHAT THE FOUR VERBS THAT QUOTE IT TAKE. It is decoded
   * ONCE, here, exactly as a real caller would -- every reply, reaction, un-reaction and tombstone
   * below passes this buffer. */
  uint8_t sent_id[ID_OCTETS] = { 0 };
  REQUIRE(hex_to_id(sent_message_id, sent_id),
          "A's message_id %s did not decode to %d octets", sent_message_id, ID_OCTETS);
  urnet_free_string(sent_info);

  step("B reads it back, and the octets are compared one at a time");
  err = NULL;
  uint64_t got = urnet_message_group_receive(group_b, ctx, &err);
  if (err != NULL) {
    show_error("receive (carried alongside whatever arrived)", err);
    err = NULL;
  }
  REQUIRE(got != 0, "B's receive answered no messages at all");
  CHECK(urnet_message_list_count(got) == 1, "B received %d messages, want 1",
        (int)urnet_message_list_count(got));
  char* info = urnet_message_list_info(got, 0);
  REQUIRE(info != NULL, "the received message had no metadata");
  printf("      %s\n", info);
  CHECK(strstr(info, "\"mine\":false") != NULL, "B read A's message as its own");
  CHECK(strstr(info, "\"body_len\":21") != NULL, "B's copy is not 21 octets");
  /* THE AGREEMENT, WHICH IS THE WHOLE PROPERTY. Two devices, two MLS states, two derivations of
   * the same 32 octets out of the same record header. An id the receiver computed from its own
   * bookkeeping would differ here, and a reply quoting A's id would name nothing on B's side. */
  char got_message_id[128] = { 0 };
  CHECK(json_string_field(info, "message_id", got_message_id, sizeof(got_message_id)),
        "B's copy of the message reported no message_id at all");
  CHECK(strcmp(got_message_id, sent_message_id) == 0,
        "A named this message %s and B named it %s; the two sides do not agree on the name of one "
        "message, so nothing that quotes an id can cross the group",
        sent_message_id, got_message_id);
  urnet_free_string(info);

  int32_t body_len = 0;
  urnet_message_list_body(got, 0, NULL, &body_len);
  CHECK(body_len == kBodyLen, "the body sized to %d octets, want %d", (int)body_len, (int)kBodyLen);
  uint8_t* body = (uint8_t*)malloc((size_t)(body_len > 0 ? body_len : 1));
  REQUIRE(body != NULL, "out of memory");
  memset(body, 0xCC, (size_t)body_len);
  int32_t body_cap = body_len;
  REQUIRE(urnet_message_list_body(got, 0, body, &body_cap), "the body would not copy out");
  int different = 0;
  for (int32_t at = 0; at < kBodyLen; at += 1) {
    if (body[at] != kBody[at]) {
      different += 1;
      fprintf(stderr, "  octet %d: got 0x%02X, sealed 0x%02X\n", (int)at, body[at], kBody[at]);
    }
  }
  CHECK(different == 0, "%d of %d octets came back different", different, (int)kBodyLen);
  CHECK(body_len == kBodyLen && different == 0,
        "the body did NOT survive the boundary byte for byte");
  free(body);
  CHECK(urnet_release(got), "releasing the message list answered false");

  step("a second receive, with nothing new: 0 handles and no error, which is the polling case");
  err = NULL;
  uint64_t nothing = urnet_message_group_receive(group_b, ctx, &err);
  CHECK(nothing == 0,
        "a receive that found nothing answered handle %llu; the ordinary poll must cost the "
        "caller no release at all",
        (unsigned long long)nothing);
  CHECK(err == NULL, "a receive that found nothing also reported an error");
  if (nothing != 0) {
    urnet_release(nothing);
  }
  if (err != NULL) {
    urnet_free_string(err);
    err = NULL;
  }
  CHECK(urnet_message_list_count(0) == 0, "counting the empty list handle did not answer 0");
  CHECK(urnet_message_list_info(0, 0) == NULL, "the empty list handle answered metadata");

  step("B answers, and A reads THAT back");
  err = NULL;
  char* reply_info = urnet_message_group_send(group_b, ctx, kReply, kReplyLen, &err);
  if (reply_info == NULL) {
    show_error("B send", err);
  }
  REQUIRE(reply_info != NULL, "B's send failed");
  /* AND THE OTHER HALF OF "IT IS A NAME": a DIFFERENT message has a different one. Without this
   * clause a binding that emitted one constant for every message would satisfy the agreement
   * check above -- both sides would read the same constant -- and every reply in the product
   * would name every message at once. */
  char reply_message_id[128] = { 0 };
  CHECK(json_string_field(reply_info, "message_id", reply_message_id, sizeof(reply_message_id)),
        "B's send reported no message_id at all");
  CHECK(is_message_id(reply_message_id),
        "B's message_id is %s, which is not 32 non-zero octets of hex", reply_message_id);
  CHECK(strcmp(reply_message_id, sent_message_id) != 0,
        "two different messages carry the same message_id %s, so an id names no particular one",
        reply_message_id);
  urnet_free_string(reply_info);
  err = NULL;
  uint64_t back = urnet_message_group_receive(group_a, ctx, &err);
  if (err != NULL) {
    show_error("A receive", err);
    err = NULL;
  }
  REQUIRE(back != 0, "A received nothing");
  CHECK(urnet_message_list_count(back) == 1,
        "A received %d messages, want 1 (its own record is already in its log)",
        (int)urnet_message_list_count(back));
  int32_t reply_len = 0;
  urnet_message_list_body(back, 0, NULL, &reply_len);
  CHECK(reply_len == kReplyLen, "the reply sized to %d, want %d", (int)reply_len, (int)kReplyLen);
  uint8_t reply_out[16];
  int32_t reply_cap = (int32_t)sizeof(reply_out);
  CHECK(urnet_message_list_body(back, 0, reply_out, &reply_cap), "the reply would not copy out");
  CHECK(reply_cap == kReplyLen && memcmp(reply_out, kReply, (size_t)kReplyLen) == 0,
        "the reply did not survive the boundary byte for byte");
  /* an index past the end is a refusal, not a read past the end */
  CHECK(!urnet_message_list_body(back, 7, reply_out, &reply_cap), "index 7 of a 1 message list was read");
  CHECK(urnet_message_list_info(back, -1) == NULL, "index -1 answered metadata");
  CHECK(urnet_release(back), "releasing A's message list answered false");

  step("SENDER_ROLE_AT_SEND crosses on a real record, and the sender's row and the receiver's agree");
  {
    /* A FOUNDED THIS GROUP, SO A IS ITS OWNER; B WAS ADDED AND THE POLICY NAMES NOBODY BUT THE
     * FOUNDER, so B is UNNAMED and reads as "member" (MASTER section 11, item 242's ruling 8).
     * That is the shape of every live group in this build, not a contrivance.
     *
     * THE PROPERTY IS AGREEMENT ACROSS THE BOUNDARY: the role a sender stamps on its own row and
     * the role a receiver captures off the record's own epoch are one value, because both are the
     * same function of the same tree at the same epoch. A build where the two could differ would
     * be two answers to one question.
     *
     * WHAT THIS DOES NOT MEASURE, said rather than implied: nothing has changed role here, so a
     * projection that read the CURRENT roster instead of the sending epoch would agree too. That
     * half -- a demotion, a send, then a promotion, with the line still carrying the old role --
     * is measured in go, over real epochs, in sdk/urmessage and sdk/cp3b. */
    uint64_t roles_a = urnet_message_group_messages(group_a);
    uint64_t roles_b = urnet_message_group_messages(group_b);
    REQUIRE(roles_a != 0 && roles_b != 0, "a log is empty");
    REQUIRE(urnet_message_list_count(roles_a) == 2 && urnet_message_list_count(roles_b) == 2,
            "the logs hold %d and %d, want 2 each",
            (int)urnet_message_list_count(roles_a), (int)urnet_message_list_count(roles_b));
    /* row 0 is A's message and row 1 is B's, in server order, at BOTH devices */
    char* a_own = urnet_message_list_info(roles_a, 0);
    char* a_copy_of_b = urnet_message_list_info(roles_a, 1);
    char* b_copy_of_a = urnet_message_list_info(roles_b, 0);
    char* b_own = urnet_message_list_info(roles_b, 1);
    REQUIRE(a_own != NULL && a_copy_of_b != NULL && b_copy_of_a != NULL && b_own != NULL,
            "a row had no metadata");
    char a_sent[64] = { 0 }, a_seen[64] = { 0 }, b_sent[64] = { 0 }, b_seen[64] = { 0 };
    CHECK(json_string_field(a_own, "sender_role_at_send", a_sent, sizeof(a_sent)),
          "A's own row carries no sender_role_at_send at all: %s", a_own);
    CHECK(json_string_field(b_copy_of_a, "sender_role_at_send", a_seen, sizeof(a_seen)),
          "B's copy of A's message carries no sender_role_at_send at all: %s", b_copy_of_a);
    CHECK(json_string_field(b_own, "sender_role_at_send", b_sent, sizeof(b_sent)),
          "B's own row carries no sender_role_at_send at all: %s", b_own);
    CHECK(json_string_field(a_copy_of_b, "sender_role_at_send", b_seen, sizeof(b_seen)),
          "A's copy of B's message carries no sender_role_at_send at all: %s", a_copy_of_b);
    printf("      A sent as %s and B read %s; B sent as %s and A read %s\n",
           a_sent, a_seen, b_sent, b_seen);
    CHECK(strcmp(a_sent, "owner") == 0, "A founded this group and its own row says %s", a_sent);
    CHECK(strcmp(b_sent, "member") == 0,
          "B is unnamed in the policy and its own row says %s, want member", b_sent);
    CHECK(strcmp(a_sent, a_seen) == 0,
          "A sent under %s and B reads that record as %s", a_sent, a_seen);
    CHECK(strcmp(b_sent, b_seen) == 0,
          "B sent under %s and A reads that record as %s", b_sent, b_seen);
    /* AND THE TWO ARE DIFFERENT VALUES IN ONE RUN, which is what an agreement between two copies
     * of one constant would also satisfy */
    CHECK(strcmp(a_sent, b_sent) != 0,
          "the owner and the member both read as %s, so this step would pass on a constant", a_sent);
    urnet_free_string(a_own);
    urnet_free_string(a_copy_of_b);
    urnet_free_string(b_copy_of_a);
    urnet_free_string(b_own);
    CHECK(urnet_release(roles_a) && urnet_release(roles_b), "releasing a log answered false");
  }

  step("the logs and the counters");
  uint64_t log_a = urnet_message_group_messages(group_a);
  uint64_t log_b = urnet_message_group_messages(group_b);
  CHECK(urnet_message_list_count(log_a) == 2, "A's log holds %d, want 2",
        (int)urnet_message_list_count(log_a));
  CHECK(urnet_message_list_count(log_b) == 2, "B's log holds %d, want 2",
        (int)urnet_message_list_count(log_b));
  CHECK(urnet_release(log_a) && urnet_release(log_b), "releasing a log answered false");
  char* stats = urnet_message_group_stats(group_b);
  REQUIRE(stats != NULL, "B's group answered no stats");
  printf("      B: %s\n", stats);
  CHECK(strstr(stats, "\"opened\":") != NULL, "the stats carry no opened counter");
  CHECK(strstr(stats, "\"own_without_copy\":") != NULL, "the stats carry no own_without_copy counter");
  /* AND THE TWO GAP COUNTERS, which are the only loud signal left for a record that could not be
   * read: a permanent post-open refusal no longer fails, so failed_open does not move, unopened
   * does not move, and receive answers no out_error. code that watched only out_error would never
   * learn a line was missing. */
  CHECK(strstr(stats, "\"gap_malformed\":") != NULL, "the stats carry no gap_malformed counter");
  CHECK(strstr(stats, "\"gap_unsupported\":") != NULL, "the stats carry no gap_unsupported counter");
  /* AND R4's THREE, which are the numbers behind sender_role_at_send: how many lines this build had
   * to HIDE because their sender was an observer that sent anyway, how many of an observer's
   * REACTIONS it refused to apply (ruling 25 -- not applying one hides nothing, because the message
   * it names is right there), and how many records opened without a readable role -- the last must
   * stay 0, here and everywhere. */
  CHECK(strstr(stats, "\"hidden_observer\":0") != NULL,
        "B has hidden an observer's message in a group that has none: %s", stats);
  CHECK(strstr(stats, "\"observer_reaction_refused\":0") != NULL,
        "B refused an observer's reaction in a group that has no observer: %s", stats);
  CHECK(strstr(stats, "\"role_undeterminable\":0") != NULL,
        "B opened a record whose sender's role it could not read: %s", stats);
  urnet_free_string(stats);

  step("the device's own group list, which is a second handle onto the same group");
  uint64_t groups_b = urnet_message_device_groups(b.device);
  REQUIRE(groups_b != 0, "B's device lists no groups");
  CHECK(urnet_message_group_list_count(groups_b) == 1, "B's device lists %d groups, want 1",
        (int)urnet_message_group_list_count(groups_b));
  uint64_t same = urnet_message_group_list_at(groups_b, 0);
  REQUIRE(same != 0, "index 0 of a 1 group list answered 0");
  CHECK(same != group_b, "the list handed back the SAME handle value, not a new one");
  CHECK(urnet_message_group_epoch(same) == urnet_message_group_epoch(group_b),
        "the two handles onto one group disagree about its epoch");
  CHECK(urnet_message_group_list_at(groups_b, 1) == 0, "index 1 of a 1 group list answered a handle");
  CHECK(urnet_release(same), "releasing the second group handle answered false");
  /* and the group is still alive under the first handle */
  CHECK(urnet_message_group_is_open(group_b), "releasing one handle closed the group under the other");
  CHECK(urnet_release(groups_b), "releasing the group list answered false");

  step("a server that holds a record back: BOTH a list AND an error come back, never one or the other");
  {
    /* three more from A, so that a page with its highest message removed still carries some. */
    for (int at = 0; at < 3; at += 1) {
      unsigned char line[8] = { 'h', 'e', 'l', 'd', ' ', 0x00, (unsigned char)('0' + at), 0x00 };
      err = NULL;
      char* info = urnet_message_group_send(group_a, ctx, line, (int32_t)sizeof(line), &err);
      if (info == NULL) {
        show_error("send during the omission case", err);
      }
      REQUIRE(info != NULL, "A could not send line %d", at);
      urnet_free_string(info);
    }
    /* the server now answers every fetch as COMPLETE while withholding its highest message and
     * still naming that record in high_water_record_id. Nothing about the AEAD can see it. */
    urnet_message_loopback_world_omit_highest(world, true);
    err = NULL;
    uint64_t partial = urnet_message_group_receive(group_b, ctx, &err);
    urnet_message_loopback_world_omit_highest(world, false);
    int32_t partial_count = urnet_message_list_count(partial);
    printf("      %d messages came back with the refusal\n", (int)partial_count);
    if (err != NULL) {
      printf("      %s\n", err);
    }
    CHECK(err != NULL,
          "the server held a record back and answered a page it called complete, and receive "
          "reported no error at all");
    CHECK(partial != 0 && partial_count > 0,
          "receive collapsed a partial answer to nothing: %d messages arrived and a caller that "
          "reads an error as 'nothing arrived' would have dropped every one of them",
          (int)partial_count);
    if (err != NULL) {
      urnet_free_string(err);
      err = NULL;
    }
    if (partial != 0) {
      urnet_release(partial);
    }
    /* and with the shape off, the held-back record is delivered rather than lost for good */
    err = NULL;
    uint64_t rest = urnet_message_group_receive(group_b, ctx, &err);
    CHECK(rest != 0, "the record the server had held back never arrived once it stopped");
    if (err != NULL) {
      show_error("the catch-up fetch", err);
      err = NULL;
    }
    if (rest != 0) {
      urnet_release(rest);
    }
  }

  /* ── EVERYTHING THE CONTENT ENVELOPE ADDED, ACROSS THE BOUNDARY ───────────────────────────
   *
   * Up to here this file has measured a conversation of plain text, which is all the projection
   * used to carry: a C caller got record_id, sender_handle, mine, sent_at_ms, body_len and
   * message_id, and the kind, the gap reason, the reply parent, the tombstone flag and the
   * reactions reached it as NOTHING AT ALL (msgrepo ledger item 236). The four steps below are
   * the other five fields, over the same real server.
   *
   * WHICH MESSAGE THEY ARE ALL ABOUT: A's first, the 21 octets that are not text, at index 0 of
   * both logs because it is the first thing either device learned. sent_message_id is its name and
   * it was captured above, at the send, which is where a real caller gets one. */
  step("a REPLY names another message, and the name crosses as reply_to_id");
  {
    uint64_t log_first = urnet_message_group_messages(group_a);
    REQUIRE(log_first != 0, "A's log is empty");
    char* first = urnet_message_list_info(log_first, 0);
    REQUIRE(first != NULL, "A's log has no row 0");
    CHECK(strstr(first, "\"body_len\":21") != NULL,
          "row 0 of A's log is not the 21 octet message these steps are about: %s", first);
    /* AND THE TWO FIELDS AN ORDINARY MESSAGE MUST NOT CLAIM. A projection that hard-coded either
     * would pass every assertion below and would mark the whole conversation. */
    CHECK(strstr(first, "\"gap\":\"\"") != NULL, "an ordinary message reports a gap: %s", first);
    CHECK(strstr(first, "\"reply_to_id\":\"\"") != NULL,
          "a message that answers nothing names a parent: %s", first);
    CHECK(strstr(first, "\"deleted\":false") != NULL, "a message nobody deleted says deleted: %s", first);
    CHECK(strstr(first, "\"reaction_count\":0") != NULL,
          "a message nobody reacted to carries reactions: %s", first);
    urnet_free_string(first);
    CHECK(urnet_release(log_first), "releasing A's log answered false");

    err = NULL;
    char* replied = urnet_message_group_send_reply(group_a, ctx, sent_id, ID_OCTETS,
                                                   kReplyText, kReplyTextLen, &err);
    if (replied == NULL) {
      show_error("send_reply", err);
      err = NULL;
    }
    REQUIRE(replied != NULL, "A could not answer its own message");
    printf("      %s\n", replied);
    char sent_reply_parent[128] = { 0 };
    CHECK(json_string_field(replied, "reply_to_id", sent_reply_parent, sizeof(sent_reply_parent)),
          "the reply the SENDER sees names no parent: %s", replied);
    CHECK(strcmp(sent_reply_parent, sent_message_id) == 0,
          "A's reply names %s and it was answering %s", sent_reply_parent, sent_message_id);
    urnet_free_string(replied);

    err = NULL;
    uint64_t arrived = urnet_message_group_receive(group_b, ctx, &err);
    if (err != NULL) {
      show_error("B receive of the reply", err);
      err = NULL;
    }
    REQUIRE(arrived != 0, "B received no reply");
    CHECK(urnet_message_list_count(arrived) == 1, "B received %d messages, want the one reply",
          (int)urnet_message_list_count(arrived));
    char* info_reply = urnet_message_list_info(arrived, 0);
    REQUIRE(info_reply != NULL, "the reply arrived with no metadata");
    printf("      %s\n", info_reply);
    /* THE KIND IS WHAT SAYS IT IS A REPLY, and it is a number rather than a name because a code
     * this build does not know still has to cross. */
    CHECK(strstr(info_reply, "\"kind\":2") != NULL,
          "the reply arrived under kind %s, want %d (URNET_MESSAGE_KIND_REPLY)",
          info_reply, URNET_MESSAGE_KIND_REPLY);
    char got_reply_parent[128] = { 0 };
    CHECK(json_string_field(info_reply, "reply_to_id", got_reply_parent, sizeof(got_reply_parent)),
          "the reply RECEIVED names no parent at all, so a ui has nothing to quote: %s", info_reply);
    CHECK(strcmp(got_reply_parent, sent_message_id) == 0,
          "B reads the reply as answering %s and A sent it answering %s; the quoted text never "
          "travels, so a parent that does not match is a reply that renders as nothing",
          got_reply_parent, sent_message_id);
    urnet_free_string(info_reply);
    CHECK(urnet_release(arrived), "releasing the reply list answered false");
  }

  step("REACTIONS are a per-message collection, with a count and an accessor");
  {
    /* B reacts to A's first message. THE RECORD ADDS NO LINE: a reaction changes another message,
     * so A's receive below answers 0 -- which is the same answer as "nothing new" and is exactly
     * why a caller has to re-read the log rather than watch the receive. */
    err = NULL;
    char* reacted = urnet_message_group_react(group_b, ctx, sent_id, ID_OCTETS, kThumbsUp, &err);
    if (reacted == NULL) {
      show_error("react", err);
      err = NULL;
    }
    REQUIRE(reacted != NULL, "B could not react");
    CHECK(strstr(reacted, "\"kind\":5") != NULL,
          "the reaction RECORD is kind %s, want %d (URNET_MESSAGE_KIND_REACTION_ADD)",
          reacted, URNET_MESSAGE_KIND_REACTION_ADD);
    urnet_free_string(reacted);

    err = NULL;
    uint64_t nothing_new = urnet_message_group_receive(group_a, ctx, &err);
    CHECK(nothing_new == 0,
          "a page carrying only a reaction delivered %d lines; a reaction is a change to another "
          "message and is not one of its own",
          (int)urnet_message_list_count(nothing_new));
    if (nothing_new != 0) {
      urnet_release(nothing_new);
    }
    if (err != NULL) {
      show_error("A receive of the reaction", err);
      err = NULL;
    }

    /* and A reacts to its own, so that the list has TWO entries and one of them is this device's */
    err = NULL;
    char* mine = urnet_message_group_react(group_a, ctx, sent_id, ID_OCTETS, kDirectHit, &err);
    if (mine == NULL) {
      show_error("A react", err);
      err = NULL;
    }
    REQUIRE(mine != NULL, "A could not react to its own message");
    urnet_free_string(mine);

    uint64_t log_r = urnet_message_group_messages(group_a);
    REQUIRE(log_r != 0, "A's log is empty");
    char* row = urnet_message_list_info(log_r, 0);
    REQUIRE(row != NULL, "A's log has no row 0");
    printf("      %s\n", row);
    CHECK(strstr(row, "\"reaction_count\":2") != NULL,
          "the row carries the wrong reaction_count: %s", row);
    urnet_free_string(row);

    CHECK(urnet_message_list_reaction_count(log_r, 0) == 2,
          "the message carries %d reactions, want the 2 that were sealed; a list that dropped its "
          "last entry would answer 1 here",
          (int)urnet_message_list_reaction_count(log_r, 0));
    /* IN THE SERVER'S OWN ORDER, which is what makes two devices draw the same row. B's ADD was
     * submitted first, so it is first. */
    char* one = urnet_message_list_reaction_info(log_r, 0, 0);
    char* two = urnet_message_list_reaction_info(log_r, 0, 1);
    REQUIRE(one != NULL && two != NULL, "a reaction in range answered no metadata");
    printf("      %s\n      %s\n", one, two);
    char emoji[64] = { 0 };
    CHECK(json_string_field(one, "emoji", emoji, sizeof(emoji)), "reaction 0 carries no emoji: %s", one);
    CHECK(strcmp(emoji, kThumbsUp) == 0, "reaction 0 is %s and B sealed %s", emoji, kThumbsUp);
    CHECK(strstr(one, "\"mine\":false") != NULL, "A reads B's reaction as its own: %s", one);
    memset(emoji, 0, sizeof(emoji));
    CHECK(json_string_field(two, "emoji", emoji, sizeof(emoji)), "reaction 1 carries no emoji: %s", two);
    CHECK(strcmp(emoji, kDirectHit) == 0, "reaction 1 is %s and A sealed %s", emoji, kDirectHit);
    CHECK(strstr(two, "\"mine\":true") != NULL, "A does not recognise its own reaction: %s", two);
    /* the reactor is a sender_handle and not a name, and the two reactions are from two members */
    char reactor_one[128] = { 0 };
    char reactor_two[128] = { 0 };
    CHECK(json_string_field(one, "sender_handle", reactor_one, sizeof(reactor_one)) &&
              json_string_field(two, "sender_handle", reactor_two, sizeof(reactor_two)),
          "a reaction names no reactor");
    CHECK(strcmp(reactor_one, reactor_two) != 0,
          "two members' reactions name one reactor %s, so a ui cannot say who reacted", reactor_one);
    urnet_free_string(one);
    urnet_free_string(two);

    /* out of range on EITHER index is a refusal and not a read past the end */
    CHECK(urnet_message_list_reaction_info(log_r, 0, 2) == NULL, "reaction 2 of 2 answered metadata");
    CHECK(urnet_message_list_reaction_info(log_r, 0, -1) == NULL, "reaction -1 answered metadata");
    CHECK(urnet_message_list_reaction_info(log_r, 99, 0) == NULL, "message 99 answered a reaction");
    CHECK(urnet_message_list_reaction_count(log_r, 99) == 0, "message 99 answered a reaction count");
    CHECK(urnet_message_list_reaction_count(0, 0) == 0, "the empty list handle answered a count");
    CHECK(urnet_message_list_reaction_info(0, 0, 0) == NULL, "the empty list handle answered a reaction");
    CHECK(urnet_release(log_r), "releasing A's log answered false");

    /* and B, which sealed one of them, sees the same two on ITS copy of the same message -- AFTER
     * it fetches A's reaction, which arrives as 0 lines for the same reason A's did */
    err = NULL;
    uint64_t b_nothing_new = urnet_message_group_receive(group_b, ctx, &err);
    CHECK(b_nothing_new == 0, "a page carrying only A's reaction delivered %d lines to B",
          (int)urnet_message_list_count(b_nothing_new));
    if (b_nothing_new != 0) {
      urnet_release(b_nothing_new);
    }
    if (err != NULL) {
      show_error("B receive of A's reaction", err);
      err = NULL;
    }
    uint64_t log_rb = urnet_message_group_messages(group_b);
    REQUIRE(log_rb != 0, "B's log is empty");
    CHECK(urnet_message_list_reaction_count(log_rb, 0) == 2,
          "B's copy of the message carries %d reactions and A's carries 2; the two devices would "
          "draw different rows",
          (int)urnet_message_list_reaction_count(log_rb, 0));
    char* b_first = urnet_message_list_reaction_info(log_rb, 0, 0);
    REQUIRE(b_first != NULL, "B's copy has no first reaction");
    CHECK(strstr(b_first, "\"mine\":true") != NULL,
          "B does not recognise the reaction B sealed: %s", b_first);
    urnet_free_string(b_first);
    CHECK(urnet_release(log_rb), "releasing B's log answered false");
  }

  step("an UN-REACTION takes back the reactor's OWN reaction and nobody else's");
  {
    /* B takes back the 👍 it sealed. THE ADD IS STILL ON THE SERVER: this is a second record
     * saying the reaction no longer stands, replayed in server order by every member, and not an
     * undo of the first. It adds no line either, for the same reason the ADD did not. */
    err = NULL;
    char* taken_back = urnet_message_group_unreact(group_b, ctx, sent_id, ID_OCTETS, kThumbsUp, &err);
    if (taken_back == NULL) {
      show_error("unreact", err);
      err = NULL;
    }
    REQUIRE(taken_back != NULL, "B could not take back its own reaction");
    printf("      %s\n", taken_back);
    CHECK(strstr(taken_back, "\"kind\":6") != NULL,
          "the un-reaction RECORD is kind %s, want %d (URNET_MESSAGE_KIND_REACTION_REMOVE)",
          taken_back, URNET_MESSAGE_KIND_REACTION_REMOVE);
    urnet_free_string(taken_back);

    /* ON B'S OWN COPY FIRST, which is the side that sealed it: one reaction left and it is A's */
    uint64_t log_u = urnet_message_group_messages(group_b);
    REQUIRE(log_u != 0, "B's log is empty");
    CHECK(urnet_message_list_reaction_count(log_u, 0) == 1,
          "B took back one of the two reactions and its own copy shows %d",
          (int)urnet_message_list_reaction_count(log_u, 0));
    char* left = urnet_message_list_reaction_info(log_u, 0, 0);
    REQUIRE(left != NULL, "the surviving reaction answered no metadata");
    char surviving[64] = { 0 };
    CHECK(json_string_field(left, "emoji", surviving, sizeof(surviving)),
          "the surviving reaction carries no emoji: %s", left);
    /* THE CONTROL, AND IT IS THE WHOLE POINT OF THIS STEP: a remove written as "drop every
     * reaction with this emoji" -- or as "drop every reaction" -- passes a one-reactor case and
     * fails here. What survives is A's, sealed by the other member, and it is still A's. */
    CHECK(strcmp(surviving, kDirectHit) == 0,
          "B's un-reaction of %s left %s standing, want A's %s", kThumbsUp, surviving, kDirectHit);
    CHECK(strstr(left, "\"mine\":false") != NULL,
          "B reads the reaction A sealed as its own after taking back its own: %s", left);
    urnet_free_string(left);
    CHECK(urnet_release(log_u), "releasing B's log answered false");

    /* AND ON A, WHICH HAS TO APPLY IT rather than having sealed it. the page carries one record
     * and no line, as both reaction pages did. */
    err = NULL;
    uint64_t no_lines = urnet_message_group_receive(group_a, ctx, &err);
    CHECK(no_lines == 0, "a page carrying only an un-reaction delivered %d lines to A",
          (int)urnet_message_list_count(no_lines));
    if (no_lines != 0) {
      urnet_release(no_lines);
    }
    if (err != NULL) {
      show_error("A receive of the un-reaction", err);
      err = NULL;
    }
    uint64_t log_ua = urnet_message_group_messages(group_a);
    REQUIRE(log_ua != 0, "A's log is empty");
    char* row = urnet_message_list_info(log_ua, 0);
    REQUIRE(row != NULL, "A's log has no row 0");
    printf("      %s\n", row);
    CHECK(strstr(row, "\"reaction_count\":1") != NULL,
          "A applied B's un-reaction and its row still says: %s", row);
    urnet_free_string(row);
    char* a_left = urnet_message_list_reaction_info(log_ua, 0, 0);
    REQUIRE(a_left != NULL, "A's surviving reaction answered no metadata");
    memset(surviving, 0, sizeof(surviving));
    CHECK(json_string_field(a_left, "emoji", surviving, sizeof(surviving)),
          "A's surviving reaction carries no emoji: %s", a_left);
    CHECK(strcmp(surviving, kDirectHit) == 0,
          "A shows %s standing after B took back %s, want its own %s",
          surviving, kThumbsUp, kDirectHit);
    CHECK(strstr(a_left, "\"mine\":true") != NULL,
          "A does not recognise the reaction it sealed itself: %s", a_left);
    urnet_free_string(a_left);
    CHECK(urnet_release(log_ua), "releasing A's log answered false");
  }

  step("the four verbs REFUSE before they seal, and the refusal crosses as out_error");
  {
    /* EVERY ONE OF THESE MUST EMIT NO RECORD. A reaction standing on an id nothing carries is a
     * record every member holds for ever waiting for a target that will not arrive; a tombstone
     * over another member's message is one every honest receiver ignores. The counter at the end
     * of this block is what says nothing was sealed -- not the NULLs, which a binding that
     * submitted and then reported an error would also return. */
    int a_before = submitted_by(group_a);
    int b_before = submitted_by(group_b);
    REQUIRE(a_before >= 0 && b_before >= 0, "a group answered no submitted counter");

    /* a message_id of the wrong WIDTH. this is the counted-octets contract itself: half an id is
     * not an id, and the length is the caller's to pass. */
    err = NULL;
    CHECK(urnet_message_group_react(group_a, ctx, sent_id, ID_OCTETS / 2, kThumbsUp, &err) == NULL,
          "a reaction naming half a message_id was sealed");
    CHECK(err != NULL, "a refused reaction reported no out_error");
    if (err != NULL) { printf("      %s\n", err); urnet_free_string(err); err = NULL; }

    /* an id nothing holds */
    uint8_t stranger[ID_OCTETS];
    memset(stranger, 0xA7, sizeof(stranger));
    err = NULL;
    CHECK(urnet_message_group_react(group_a, ctx, stranger, ID_OCTETS, kThumbsUp, &err) == NULL,
          "a reaction naming a message nothing holds was sealed");
    CHECK(err != NULL, "a reaction on an unknown id reported no out_error");
    if (err != NULL) { printf("      %s\n", err); urnet_free_string(err); err = NULL; }

    /* an empty emoji, which is not a reaction at all */
    err = NULL;
    CHECK(urnet_message_group_react(group_a, ctx, sent_id, ID_OCTETS, "", &err) == NULL,
          "a reaction with no emoji was sealed");
    if (err != NULL) { urnet_free_string(err); err = NULL; }

    /* and T-b from the send side: B may not delete a message A sealed */
    err = NULL;
    CHECK(urnet_message_group_delete(group_b, ctx, sent_id, ID_OCTETS, &err) == NULL,
          "B deleted a message A sealed");
    CHECK(err != NULL, "a refused tombstone reported no out_error");
    if (err != NULL) { printf("      %s\n", err); urnet_free_string(err); err = NULL; }

    /* AND NOTHING WAS SUBMITTED BY ANY OF THE FOUR, on either device. This is the assertion the
     * NULL returns cannot make: a binding that sealed, submitted and then reported an error would
     * answer NULL exactly the same way, having spent a stream index and an mls generation. */
    CHECK(submitted_by(group_a) == a_before,
          "A's three refusals submitted %d record(s)", submitted_by(group_a) - a_before);
    CHECK(submitted_by(group_b) == b_before,
          "B's refused tombstone submitted %d record(s)", submitted_by(group_b) - b_before);
  }

  step("a TOMBSTONE marks the message on both sides, and the body is still there");
  {
    err = NULL;
    char* buried = urnet_message_group_delete(group_a, ctx, sent_id, ID_OCTETS, &err);
    if (buried == NULL) {
      show_error("delete", err);
      err = NULL;
    }
    REQUIRE(buried != NULL, "A could not delete its own message");
    CHECK(strstr(buried, "\"kind\":4") != NULL,
          "the tombstone RECORD is kind %s, want %d (URNET_MESSAGE_KIND_TOMBSTONE)",
          buried, URNET_MESSAGE_KIND_TOMBSTONE);
    urnet_free_string(buried);

    uint64_t log_d = urnet_message_group_messages(group_a);
    REQUIRE(log_d != 0, "A's log is empty");
    char* row = urnet_message_list_info(log_d, 0);
    REQUIRE(row != NULL, "A's log has no row 0");
    printf("      %s\n", row);
    CHECK(strstr(row, "\"deleted\":true") != NULL, "A's own tombstone did not mark its message: %s", row);
    /* THE BODY IS STILL HERE AND STILL COMES BACK. the library marks the line and keeps the text
     * -- it refuses to be the layer that throws away a user's data on a peer's say-so, and the
     * record is on the server either way -- so "deleted" is how a ui learns it has a decision to
     * make, and NOT the library having made it. */
    CHECK(strstr(row, "\"body_len\":21") != NULL, "a deleted message lost its body_len: %s", row);
    urnet_free_string(row);
    int32_t still = 0;
    urnet_message_list_body(log_d, 0, NULL, &still);
    CHECK(still == kBodyLen, "a deleted message's body sized to %d, want %d", (int)still, (int)kBodyLen);
    CHECK(urnet_release(log_d), "releasing A's log answered false");

    /* and on B, which has to APPLY the tombstone rather than having sealed it */
    err = NULL;
    uint64_t no_lines = urnet_message_group_receive(group_b, ctx, &err);
    CHECK(no_lines == 0, "a page carrying only a tombstone delivered %d lines",
          (int)urnet_message_list_count(no_lines));
    if (no_lines != 0) {
      urnet_release(no_lines);
    }
    if (err != NULL) {
      show_error("B receive of the tombstone", err);
      err = NULL;
    }
    uint64_t log_db = urnet_message_group_messages(group_b);
    REQUIRE(log_db != 0, "B's log is empty");
    char* b_row = urnet_message_list_info(log_db, 0);
    REQUIRE(b_row != NULL, "B's log has no row 0");
    CHECK(strstr(b_row, "\"deleted\":true") != NULL,
          "B did not apply the sender's own tombstone: %s", b_row);
    urnet_free_string(b_row);
    CHECK(urnet_release(log_db), "releasing B's log answered false");
  }

  step("the ROSTER and the two ROLE VERBS: the owner promotes, the member is refused by kind, nothing moves");
  {
    /* WHAT THIS IS FOR. The role model (MASTER section 11) judges every commit on both sides
     * underneath this abi, and until these exports a C caller could see none of it. This drives
     * the whole of what crosses: the roster with a role per row and one row marked mine, this
     * device's own role, a REFUSED verb whose refusal is a KIND a caller can branch on and after
     * which nothing has moved for anybody, three INVALID requests refused by name, and the
     * owner's promotion of B -- which B learns of through a real receive and which both rosters
     * then agree on. LOST cannot be produced here (it takes a race); the go side holds its
     * projection and cp3b's lost-race case holds the verb. */
    char a_role[32] = { 0 }, b_role[32] = { 0 };
    char a_identity[256] = { 0 }, b_identity[256] = { 0 }, b_identity_at_b[256] = { 0 };
    char other_role[32] = { 0 }, other_identity[256] = { 0 };

    /* drain A first, so that "nothing arrived" below is about the refused verb and not about
     * the tombstone step's own record coming back */
    err = NULL;
    uint64_t drained = urnet_message_group_receive(group_a, ctx, &err);
    if (drained != 0) { urnet_release(drained); }
    if (err != NULL) { urnet_free_string(err); err = NULL; }

    uint64_t roster_a = urnet_message_group_members(group_a, &err);
    if (roster_a == 0) {
      show_error("A members", err);
      err = NULL;
    }
    REQUIRE(roster_a != 0, "A's group answered no roster");
    CHECK(urnet_message_member_list_count(roster_a) == 2, "A's roster holds %d rows, want 2",
          (int)urnet_message_member_list_count(roster_a));
    for (int32_t at = 0; at < urnet_message_member_list_count(roster_a); at += 1) {
      char* row = urnet_message_member_list_info(roster_a, at);
      REQUIRE(row != NULL, "A's roster row %d has no json", (int)at);
      printf("      A sees %s\n", row);
      CHECK(strstr(row, "\"leaf_index\":") != NULL, "a roster row carries no leaf_index: %s", row);
      CHECK(strstr(row, "\"sender_handle\":\"") != NULL, "a roster row carries no sender_handle: %s", row);
      urnet_free_string(row);
    }
    CHECK(urnet_message_member_list_info(roster_a, 2) == NULL, "index 2 of a 2 row roster answered json");
    CHECK(urnet_message_member_list_info(roster_a, -1) == NULL, "index -1 answered json");
    REQUIRE(roster_row(roster_a, true, a_role, sizeof(a_role), a_identity, sizeof(a_identity)),
            "A's roster does not mark exactly one row as A's own");
    REQUIRE(roster_row(roster_a, false, b_role, sizeof(b_role), b_identity, sizeof(b_identity)),
            "A's roster does not hold exactly one row that is not A's own");
    CHECK(strcmp(a_role, "owner") == 0, "A, the founder, reads its own role as %s, want owner", a_role);
    CHECK(strcmp(b_role, "member") == 0, "A reads the unnamed B as %s, want member", b_role);
    CHECK(strlen(b_identity) > 0 && strcmp(a_identity, b_identity) != 0,
          "the two rows carry the same identity_pub %s", a_identity);
    CHECK(urnet_release(roster_a), "releasing A's roster answered false");
    CHECK(urnet_message_member_list_count(0) == 0, "the zero roster handle answered a count");
    CHECK(urnet_message_member_list_info(0, 0) == NULL, "the zero roster handle answered json");

    /* my_role, on both, is the same reading each roster gave */
    err = NULL;
    char* my_role_a = urnet_message_group_my_role(group_a, &err);
    REQUIRE(my_role_a != NULL, "A answered no role for itself");
    CHECK(strcmp(my_role_a, "owner") == 0, "A's my_role is %s, want owner", my_role_a);
    urnet_free_string(my_role_a);
    char* my_role_b = urnet_message_group_my_role(group_b, &err);
    REQUIRE(my_role_b != NULL, "B answered no role for itself");
    CHECK(strcmp(my_role_b, "member") == 0, "B's my_role is %s, want member", my_role_b);
    urnet_free_string(my_role_b);
    CHECK(urnet_message_group_my_role(0, &err) == NULL, "the zero handle answered a role");

    /* THE MEMBER'S VERB OVER THE OWNER IS REFUSED BY KIND, AND NOTHING MOVES. B is a member;
     * set_role is an admin's or the owner's verb whatever it asks for (ruling 15), so this is
     * REFUSED before anything is built: B's epoch and submitted count do not move, A has nothing
     * to fetch, and B's commit_refused_own moved by exactly one. */
    uint64_t epoch_b_before = urnet_message_group_epoch(group_b);
    int submitted_b_before = submitted_by(group_b);
    int refused_b_before = counter_of(group_b, "commit_refused_own");
    err = NULL;
    int32_t kind = urnet_message_group_set_role(group_b, ctx, a_identity, "member", &err);
    printf("      B's set_role over the owner answered kind %d: %s\n", (int)kind, err != NULL ? err : "(no error text)");
    CHECK(kind == URNET_MESSAGE_COMMIT_REFUSED,
          "B's set_role over the owner answered kind %d, want %d (REFUSED)", (int)kind, URNET_MESSAGE_COMMIT_REFUSED);
    CHECK(err != NULL, "a REFUSED verb set no out_error");
    if (err != NULL) { urnet_free_string(err); err = NULL; }
    CHECK(urnet_message_group_epoch(group_b) == epoch_b_before,
          "B's epoch moved from %llu to %llu over a refused verb", (unsigned long long)epoch_b_before,
          (unsigned long long)urnet_message_group_epoch(group_b));
    CHECK(submitted_by(group_b) == submitted_b_before,
          "B submitted %d record(s) over a refused verb", submitted_by(group_b) - submitted_b_before);
    CHECK(counter_of(group_b, "commit_refused_own") == refused_b_before + 1,
          "B's commit_refused_own went %d -> %d over one refused verb, want one more",
          refused_b_before, counter_of(group_b, "commit_refused_own"));
    err = NULL;
    uint64_t nothing_for_a = urnet_message_group_receive(group_a, ctx, &err);
    CHECK(nothing_for_a == 0 && err == NULL,
          "A fetched something after B's refused verb, so something was published");
    if (nothing_for_a != 0) { urnet_release(nothing_for_a); }
    if (err != NULL) { urnet_free_string(err); err = NULL; }
    CHECK(urnet_message_group_epoch(group_a) == epoch_b_before,
          "A's epoch is %llu after B's refused verb", (unsigned long long)urnet_message_group_epoch(group_a));

    /* THREE INVALID REQUESTS, refused by name before any rule, and counted nowhere */
    int refused_a_before = counter_of(group_a, "commit_refused_own");
    err = NULL;
    kind = urnet_message_group_set_role(group_a, ctx, b_identity, "owner", &err);
    CHECK(kind == URNET_MESSAGE_COMMIT_INVALID && err != NULL,
          "set_role to \"owner\" answered kind %d, want %d (INVALID) with out_error", (int)kind, URNET_MESSAGE_COMMIT_INVALID);
    if (err != NULL) { urnet_free_string(err); err = NULL; }
    kind = urnet_message_group_set_role(group_a, ctx, "not hex at all", "admin", &err);
    CHECK(kind == URNET_MESSAGE_COMMIT_INVALID && err != NULL,
          "set_role with a non-hex identity answered kind %d, want %d (INVALID) with out_error", (int)kind, URNET_MESSAGE_COMMIT_INVALID);
    if (err != NULL) { urnet_free_string(err); err = NULL; }
    kind = urnet_message_group_transfer_ownership(group_a, ctx, a_identity, &err);
    CHECK(kind == URNET_MESSAGE_COMMIT_INVALID && err != NULL,
          "a transfer to the current owner answered kind %d, want %d (INVALID) with out_error", (int)kind, URNET_MESSAGE_COMMIT_INVALID);
    if (err != NULL) { urnet_free_string(err); err = NULL; }
    CHECK(counter_of(group_a, "commit_refused_own") == refused_a_before,
          "an INVALID request was counted as a role refusal");
    /* and the zero handle is FAILED with out_error left NULL, which is this abi's convention */
    kind = urnet_message_group_set_role(0, ctx, b_identity, "admin", &err);
    CHECK(kind == URNET_MESSAGE_COMMIT_FAILED && err == NULL,
          "set_role on handle 0 answered kind %d with out_error %s, want %d (FAILED) and NULL",
          (int)kind, err != NULL ? err : "NULL", URNET_MESSAGE_COMMIT_FAILED);
    if (err != NULL) { urnet_free_string(err); err = NULL; }
    CHECK(urnet_message_group_epoch(group_a) == epoch_b_before, "an INVALID request moved A's epoch");

    /* THE OWNER PROMOTES B: OK, the epoch moves, and B learns of it through a real receive */
    err = NULL;
    kind = urnet_message_group_set_role(group_a, ctx, b_identity, "admin", &err);
    if (kind != URNET_MESSAGE_COMMIT_OK) {
      show_error("A's promotion of B", err);
      err = NULL;
    }
    REQUIRE(kind == URNET_MESSAGE_COMMIT_OK, "A's set_role promoting B answered kind %d", (int)kind);
    CHECK(err == NULL, "an OK verb set an out_error");
    CHECK(urnet_message_group_epoch(group_a) == epoch_b_before + 1,
          "A is at epoch %llu after its promotion commit, want %llu",
          (unsigned long long)urnet_message_group_epoch(group_a), (unsigned long long)(epoch_b_before + 1));
    err = NULL;
    uint64_t commit_only = urnet_message_group_receive(group_b, ctx, &err);
    if (err != NULL) {
      show_error("B's receive of the promotion", err);
      err = NULL;
    }
    CHECK(commit_only == 0, "a page carrying only a commit delivered %d lines",
          (int)urnet_message_list_count(commit_only));
    if (commit_only != 0) { urnet_release(commit_only); }
    CHECK(urnet_message_group_epoch(group_b) == epoch_b_before + 1,
          "B did not follow the promotion: B is at epoch %llu, A at %llu",
          (unsigned long long)urnet_message_group_epoch(group_b), (unsigned long long)urnet_message_group_epoch(group_a));
    err = NULL;
    my_role_b = urnet_message_group_my_role(group_b, &err);
    REQUIRE(my_role_b != NULL, "B answered no role for itself after the promotion");
    CHECK(strcmp(my_role_b, "admin") == 0, "B's my_role after the promotion is %s, want admin", my_role_b);
    urnet_free_string(my_role_b);
    uint64_t roster_b = urnet_message_group_members(group_b, &err);
    REQUIRE(roster_b != 0, "B's group answered no roster after the promotion");
    REQUIRE(roster_row(roster_b, true, b_role, sizeof(b_role), b_identity_at_b, sizeof(b_identity_at_b)),
            "B's roster does not mark exactly one row as B's own");
    REQUIRE(roster_row(roster_b, false, other_role, sizeof(other_role), other_identity, sizeof(other_identity)),
            "B's roster does not hold exactly one row that is not B's own");
    CHECK(strcmp(b_role, "admin") == 0, "B's own roster row says %s after the promotion, want admin", b_role);
    /* THE IDENTITY A NAMED IS THE ONE B READS AS ITS OWN, which is what makes identity_pub a name
     * a verb can be given: the promotion landed on the row B calls mine */
    CHECK(strcmp(b_identity_at_b, b_identity) == 0,
          "A promoted %s and B's own row is %s", b_identity, b_identity_at_b);
    CHECK(strcmp(other_role, "owner") == 0, "B reads A as %s, want owner", other_role);
    CHECK(strcmp(other_identity, a_identity) == 0,
          "B's roster names A as %s and A's names itself %s", other_identity, a_identity);
    CHECK(urnet_release(roster_b), "releasing B's roster answered false");
    roster_a = urnet_message_group_members(group_a, &err);
    REQUIRE(roster_a != 0, "A's group answered no roster after the promotion");
    REQUIRE(roster_row(roster_a, false, other_role, sizeof(other_role), other_identity, sizeof(other_identity)),
            "A's roster lost B's row");
    CHECK(strcmp(other_role, "admin") == 0, "A reads B as %s after promoting it, want admin", other_role);
    CHECK(urnet_release(roster_a), "releasing A's roster answered false");
    printf("      A promoted B: both rosters read A owner / B admin at epoch %llu\n",
           (unsigned long long)urnet_message_group_epoch(group_b));

    /* THE SAME ROLE AGAIN IS OK AND MOVES NOTHING (ruling 15): no commit, no epoch, nothing to
     * fetch */
    err = NULL;
    kind = urnet_message_group_set_role(group_a, ctx, b_identity, "admin", &err);
    CHECK(kind == URNET_MESSAGE_COMMIT_OK && err == NULL,
          "naming B admin again answered kind %d, want %d (OK) and no error", (int)kind, URNET_MESSAGE_COMMIT_OK);
    if (err != NULL) { urnet_free_string(err); err = NULL; }
    CHECK(urnet_message_group_epoch(group_a) == epoch_b_before + 1,
          "a same-role set_role moved A's epoch to %llu", (unsigned long long)urnet_message_group_epoch(group_a));
    uint64_t nothing_for_b = urnet_message_group_receive(group_b, ctx, &err);
    CHECK(nothing_for_b == 0 && err == NULL, "B fetched something after a same-role set_role");
    if (nothing_for_b != 0) { urnet_release(nothing_for_b); }
    if (err != NULL) { urnet_free_string(err); err = NULL; }

    /* AND AN ADMIN STILL MAY NOT TOUCH THE ADMIN SET: B demoting itself is REFUSED by the rule
     * that names it, which is the predicate and not the caller check -- so the refusal kind
     * covers both arms of the send-side decision. */
    refused_b_before = counter_of(group_b, "commit_refused_own");
    err = NULL;
    kind = urnet_message_group_set_role(group_b, ctx, b_identity, "member", &err);
    printf("      B's demotion of itself as an admin answered kind %d: %s\n", (int)kind, err != NULL ? err : "(no error text)");
    CHECK(kind == URNET_MESSAGE_COMMIT_REFUSED && err != NULL,
          "an admin's change to the admin set answered kind %d, want %d (REFUSED) with out_error", (int)kind, URNET_MESSAGE_COMMIT_REFUSED);
    if (err != NULL) { urnet_free_string(err); err = NULL; }
    CHECK(counter_of(group_b, "commit_refused_own") == refused_b_before + 1,
          "B's commit_refused_own did not move over the refused demotion");
    CHECK(urnet_message_group_epoch(group_b) == epoch_b_before + 1 &&
          urnet_message_group_epoch(group_a) == epoch_b_before + 1,
          "an epoch moved over B's refused demotion: A %llu, B %llu",
          (unsigned long long)urnet_message_group_epoch(group_a), (unsigned long long)urnet_message_group_epoch(group_b));
  }

  step("REMOVE_MEMBER: four INVALID requests by name, then the owner takes the admin out in ONE "
       "commit and the removed device can no longer follow the group");
  {
    /* THE IDENTITIES ARE RE-READ OFF THE ROSTER RATHER THAN CARRIED DOWN, because the roster is
     * the whole of what a caller has: identity_pub_hex is the field a member info carries and is
     * passed back as is. One call per PERSON, not one per device.
     *
     * WHAT IS NOT HERE, said rather than glossed: a REFUSED removal. In a two-member group every
     * removal a non-owner could ask for names the OWNER, which is INVALID by name and not a role
     * answer, so REFUSED needs a third member -- the kind projection is held in Go
     * (TestTheCommitKindProjectionTellsTheThreeAnswersApart) and the refusal end to end in cp3b. */
    char own_role[32], their_role[32], own_hex[160], their_hex[160];
    char* err = NULL;
    uint64_t roster = urnet_message_group_members(group_a, &err);
    REQUIRE(roster != 0, "A's group answered no roster before the removal");
    REQUIRE(roster_row(roster, true, own_role, sizeof(own_role), own_hex, sizeof(own_hex)),
            "A's roster does not mark exactly one row as A's own");
    REQUIRE(roster_row(roster, false, their_role, sizeof(their_role), their_hex, sizeof(their_hex)),
            "A's roster does not hold exactly one row that is not A's own");
    CHECK(urnet_release(roster), "releasing A's roster answered false");
    CHECK(strcmp(own_role, "owner") == 0, "A reads its own role as %s before the removal", own_role);
    CHECK(strcmp(their_role, "admin") == 0, "A reads B as %s before the removal, want admin", their_role);

    uint64_t epoch_before = urnet_message_group_epoch(group_a);
    int refused_a_pre = counter_of(group_a, "commit_refused_own");
    int refused_b_pre = counter_of(group_b, "commit_refused_own");
    int submitted_a_pre = submitted_by(group_a);
    int32_t kind = 0;

    /* (1) THE OWNER'S OWN IDENTITY, asked by the owner: INVALID, and the text names
     * transfer_ownership. MASTER section 11 refuses a leave to an OWNER until the group has been
     * handed over, so this is not "ask an admin" either. */
    err = NULL;
    kind = urnet_message_group_remove_member(group_a, ctx, own_hex, &err);
    printf("      A asking to remove itself answered kind %d: %s\n", (int)kind, err != NULL ? err : "(no error text)");
    CHECK(kind == URNET_MESSAGE_COMMIT_INVALID && err != NULL,
          "A removing its own (owner) identity answered kind %d, want %d (INVALID) with out_error",
          (int)kind, URNET_MESSAGE_COMMIT_INVALID);
    if (err != NULL) { urnet_free_string(err); err = NULL; }

    /* (2) THE OWNER'S IDENTITY, asked by the admin: INVALID for the same reason, whoever asks */
    kind = urnet_message_group_remove_member(group_b, ctx, own_hex, &err);
    CHECK(kind == URNET_MESSAGE_COMMIT_INVALID && err != NULL,
          "B removing the owner answered kind %d, want %d (INVALID) with out_error",
          (int)kind, URNET_MESSAGE_COMMIT_INVALID);
    if (err != NULL) { urnet_free_string(err); err = NULL; }

    /* (3) THIS DEVICE'S OWN IDENTITY: leaving is a product flow and not this call */
    kind = urnet_message_group_remove_member(group_b, ctx, their_hex, &err);
    printf("      B asking to remove itself answered kind %d: %s\n", (int)kind, err != NULL ? err : "(no error text)");
    CHECK(kind == URNET_MESSAGE_COMMIT_INVALID && err != NULL,
          "B removing its own identity answered kind %d, want %d (INVALID) with out_error",
          (int)kind, URNET_MESSAGE_COMMIT_INVALID);
    if (err != NULL) { urnet_free_string(err); err = NULL; }

    /* (4) AN IDENTITY NO LEAF CARRIES, and an identity_pub_hex that is not hex at all */
    kind = urnet_message_group_remove_member(group_a, ctx,
        "0000000000000000000000000000000000000000000000000000000000000000", &err);
    CHECK(kind == URNET_MESSAGE_COMMIT_INVALID && err != NULL,
          "removing an identity that holds no leaf answered kind %d, want %d (INVALID) with out_error",
          (int)kind, URNET_MESSAGE_COMMIT_INVALID);
    if (err != NULL) { urnet_free_string(err); err = NULL; }
    kind = urnet_message_group_remove_member(group_a, ctx, "not hex at all", &err);
    CHECK(kind == URNET_MESSAGE_COMMIT_INVALID && err != NULL,
          "remove_member with a non-hex identity answered kind %d, want %d (INVALID) with out_error",
          (int)kind, URNET_MESSAGE_COMMIT_INVALID);
    if (err != NULL) { urnet_free_string(err); err = NULL; }

    /* NOTHING WAS BUILT AND NOTHING WAS COUNTED AS A ROLE REFUSAL: INVALID is a caller bug */
    CHECK(counter_of(group_a, "commit_refused_own") == refused_a_pre &&
          counter_of(group_b, "commit_refused_own") == refused_b_pre,
          "an INVALID remove_member was counted as a role refusal: A %d -> %d, B %d -> %d",
          refused_a_pre, counter_of(group_a, "commit_refused_own"),
          refused_b_pre, counter_of(group_b, "commit_refused_own"));
    CHECK(submitted_by(group_a) == submitted_a_pre,
          "A submitted %d record(s) over five INVALID requests", submitted_by(group_a) - submitted_a_pre);
    CHECK(urnet_message_group_epoch(group_a) == epoch_before &&
          urnet_message_group_epoch(group_b) == epoch_before,
          "an epoch moved over the INVALID requests: A %llu, B %llu",
          (unsigned long long)urnet_message_group_epoch(group_a),
          (unsigned long long)urnet_message_group_epoch(group_b));

    /* and the zero handle is FAILED with out_error left NULL, this abi's convention */
    kind = urnet_message_group_remove_member(0, ctx, their_hex, &err);
    CHECK(kind == URNET_MESSAGE_COMMIT_FAILED && err == NULL,
          "remove_member on handle 0 answered kind %d with out_error %s, want %d (FAILED) and NULL",
          (int)kind, err != NULL ? err : "NULL", URNET_MESSAGE_COMMIT_FAILED);
    if (err != NULL) { urnet_free_string(err); err = NULL; }

    /* THE OWNER REMOVES THE ADMIN: OK, ONE epoch, and A's roster is one row -- its own */
    kind = urnet_message_group_remove_member(group_a, ctx, their_hex, &err);
    if (kind != URNET_MESSAGE_COMMIT_OK) {
      show_error("A's removal of B", err);
      err = NULL;
    }
    REQUIRE(kind == URNET_MESSAGE_COMMIT_OK, "A's remove_member of B answered kind %d", (int)kind);
    CHECK(err == NULL, "an OK verb set an out_error");
    CHECK(urnet_message_group_epoch(group_a) == epoch_before + 1,
          "A is at epoch %llu after ONE removal commit, want %llu",
          (unsigned long long)urnet_message_group_epoch(group_a), (unsigned long long)(epoch_before + 1));
    err = NULL;
    roster = urnet_message_group_members(group_a, &err);
    REQUIRE(roster != 0, "A's group answered no roster after the removal");
    CHECK(urnet_message_member_list_count(roster) == 1,
          "A reads %d member(s) after removing the only other one, want 1",
          (int)urnet_message_member_list_count(roster));
    CHECK(roster_row(roster, true, own_role, sizeof(own_role), own_hex, sizeof(own_hex)) &&
          strcmp(own_role, "owner") == 0,
          "A's own row after the removal reads %s", own_role);
    CHECK(!roster_row(roster, false, their_role, sizeof(their_role), their_hex, sizeof(their_hex)),
          "A's roster still holds a row that is not its own after the removal");
    CHECK(urnet_release(roster), "releasing A's roster answered false");

    /* AND THE REMOVED DEVICE CANNOT FOLLOW THE EPOCH ITS OWN REMOVAL OPENED. Its fetch still
     * works -- it holds the keys of the epoch it was removed AT -- and the commit it is served
     * cannot be applied, so it stays where it was. */
    err = NULL;
    uint64_t nothing = urnet_message_group_receive(group_b, ctx, &err);
    printf("      the removed device's receive answered: %s\n", err != NULL ? err : "(no error text)");
    CHECK(err != NULL, "the removed device's receive came back clean, so it followed its own removal");
    if (nothing != 0) { urnet_release(nothing); }
    if (err != NULL) { urnet_free_string(err); err = NULL; }
    CHECK(urnet_message_group_epoch(group_b) == epoch_before,
          "the removed device is at epoch %llu, want %llu: it followed its own removal",
          (unsigned long long)urnet_message_group_epoch(group_b), (unsigned long long)epoch_before);

    /* AND THE STATE IT IS LEFT IN CROSSES AS SOMETHING A SCREEN CAN RENDER -- LEDGER RULING 52.
     * The receive above answers an out_error, which a C caller cannot branch on; this is the half
     * that makes "you are no longer in this group" a STATE rather than a sentence. The SURVIVOR is
     * asked the same question in the same breath, so `removed` cannot be a field that is always
     * true, and the epoch is held against the one B was a member AT rather than the one its removal
     * opened. Read with no call having had to fail first: A has never seen an error here. */
    char* removal_b = urnet_message_group_removal(group_b);
    char* removal_a = urnet_message_group_removal(group_a);
    REQUIRE(removal_b != NULL && removal_a != NULL, "the removal projection answered NULL for a live group");
    printf("      removed device: %s\n      the survivor:   %s\n", removal_b, removal_a);
    CHECK(strstr(removal_b, "\"removed\":true") != NULL,
          "the removed device's projection does not say so: %s", removal_b);
    CHECK(strstr(removal_a, "\"removed\":false") != NULL,
          "CONTROL FAILED: the SURVIVOR's projection does not say removed false, so the clause "
          "above is satisfied by a field that is always true: %s", removal_a);
    char removed_epoch[48] = { 0 };
    snprintf(removed_epoch, sizeof(removed_epoch), "\"removed_epoch\":%llu",
             (unsigned long long)epoch_before);
    CHECK(strstr(removal_b, removed_epoch) != NULL,
          "the removed device's projection does not carry %s: %s", removed_epoch, removal_b);
    urnet_free_string(removal_b);
    urnet_free_string(removal_a);

    /* AND ITS SEND DOOR REFUSES, by the same state rather than with an mls sentence about a data
     * structure: before ruling 52 this answered "an application record's inner MLS frame did not
     * open: mls: the group is closed and its epoch secrets have been zeroized". */
    err = NULL;
    const char* after_removal = "a line from somebody who is not in this group any more";
    char* refused = urnet_message_group_send(group_b, ctx,
        (const uint8_t*)after_removal, (int32_t)strlen(after_removal), &err);
    printf("      the removed device's send answered: %s\n", err != NULL ? err : "(no error text)");
    CHECK(refused == NULL && err != NULL, "the removed device's send was accepted");
    if (refused != NULL) { urnet_free_string(refused); }
    if (err != NULL) { urnet_free_string(err); err = NULL; }

    printf("      A removed B in one commit: A alone at epoch %llu, B still at %llu\n",
           (unsigned long long)urnet_message_group_epoch(group_a),
           (unsigned long long)urnet_message_group_epoch(group_b));
  }

  step("A GAP IS NOT A MESSAGE WITH NO TEXT, which is the whole of ledger item 236");
  {
    /* the four entries are built in Go -- see urnet_message_loopback_gap_list, and the reason
     * nothing here can seal a malformed record -- so what this step measures is the BOUNDARY and
     * not the walk: that a gap reaches a C caller as something it can tell apart. */
    uint64_t gaps = urnet_message_loopback_gap_list();
    REQUIRE(gaps != 0, "the gap list answered 0");
    CHECK(urnet_message_list_count(gaps) == 4, "the gap list holds %d entries, want 4",
          (int)urnet_message_list_count(gaps));
    char* message = urnet_message_list_info(gaps, 0);
    char* unsupported = urnet_message_list_info(gaps, 1);
    char* malformed = urnet_message_list_info(gaps, 2);
    char* out_of_window = urnet_message_list_info(gaps, 3);
    REQUIRE(message != NULL && unsupported != NULL && malformed != NULL && out_of_window != NULL,
            "an entry had no metadata");
    printf("      %s\n      %s\n      %s\n      %s\n", message, unsupported, malformed, out_of_window);

    CHECK(strstr(message, "\"gap\":\"\"") != NULL, "a message reports a gap: %s", message);
    /* BOTH GAPS ARE body_len 0 AND SO IS A MESSAGE NOBODY PUT TEXT IN. that is the whole defect:
     * without this field the three below are one value to a C caller, and the two gaps are two
     * different sentences to a user -- one offers an upgrade and the other must not. */
    CHECK(strstr(unsupported, "\"body_len\":0") != NULL && strstr(malformed, "\"body_len\":0") != NULL,
          "a gap carries a body, so this case is measuring something else");
    char reason[64] = { 0 };
    CHECK(json_string_field(unsupported, "gap", reason, sizeof(reason)), "no gap field: %s", unsupported);
    CHECK(strcmp(reason, URNET_MESSAGE_GAP_UNSUPPORTED) == 0,
          "an unknown kind reports gap %s, want %s", reason, URNET_MESSAGE_GAP_UNSUPPORTED);
    memset(reason, 0, sizeof(reason));
    CHECK(json_string_field(malformed, "gap", reason, sizeof(reason)), "no gap field: %s", malformed);
    CHECK(strcmp(reason, URNET_MESSAGE_GAP_MALFORMED) == 0,
          "a malformed record reports gap %s, want %s", reason, URNET_MESSAGE_GAP_MALFORMED);
    /* AND THE KIND ON A GAP IS THE CODE IT ARRIVED UNDER AND NOT WHAT IT IS: entry 2 is a
     * malformed REPLY, so a caller that branched on kind would draw an empty reply for it. */
    CHECK(strstr(malformed, "\"kind\":2") != NULL,
          "the malformed entry lost the code it arrived under: %s", malformed);
    CHECK(strstr(unsupported, "\"kind\":3") != NULL,
          "the unsupported entry lost the code it arrived under: %s", unsupported);
    /* a gap still has its position and its name, which is what makes it an entry rather than a
     * hole: a ui draws it in order and a later reply can still quote it */
    char gap_id[128] = { 0 };
    CHECK(json_string_field(unsupported, "message_id", gap_id, sizeof(gap_id)) && is_message_id(gap_id),
          "the gap carries no message_id, so it is a hole and not an entry: %s", unsupported);

    /* ── SENDER_ROLE_AT_SEND, AND THE RULE THAT DECIDES WHEN IT IS EMPTY (item 242's R4) ────
     *
     * NON-EMPTY ON EXACTLY THE ROWS THAT OPENED. The message and the two gaps above all opened --
     * their AEADs were satisfied and their frames were signed -- so each says what role its
     * sender held AT THE EPOCH IT SEALED THE RECORD AT. The fourth did not open at all: it was
     * sealed at an epoch no key schedule on this device reaches, so the only thing naming its
     * sender is the record's own unauthenticated claim and there is no epoch to read a role in.
     *
     * THE THREE VALUES ARE THREE DIFFERENT ROLES, which is what a projection that carried one
     * constant would fail. A malformed record from an OBSERVER is both things at once: a closed
     * placeholder to draw, and a sender whose messages this build hides. */
    char role[64] = { 0 };
    CHECK(json_string_field(message, "sender_role_at_send", role, sizeof(role)) &&
              strcmp(role, "owner") == 0,
          "the message reports sender_role_at_send %s, want owner: %s", role, message);
    memset(role, 0, sizeof(role));
    CHECK(json_string_field(unsupported, "sender_role_at_send", role, sizeof(role)) &&
              strcmp(role, "member") == 0,
          "the unsupported gap reports sender_role_at_send %s, want member: %s", role, unsupported);
    memset(role, 0, sizeof(role));
    CHECK(json_string_field(malformed, "sender_role_at_send", role, sizeof(role)) &&
              strcmp(role, "observer") == 0,
          "the malformed gap reports sender_role_at_send %s, want observer: %s", role, malformed);
    /* and the record that never opened carries NO role, which is the "only if" half */
    CHECK(strstr(out_of_window, "\"sender_role_at_send\":\"\"") != NULL,
          "a record that never opened claims a role for its sender: %s", out_of_window);
    memset(reason, 0, sizeof(reason));
    CHECK(json_string_field(out_of_window, "gap", reason, sizeof(reason)), "no gap field: %s", out_of_window);
    CHECK(strcmp(reason, URNET_MESSAGE_GAP_OUT_OF_WINDOW) == 0,
          "a record from an unreachable epoch reports gap %s, want %s",
          reason, URNET_MESSAGE_GAP_OUT_OF_WINDOW);

    urnet_free_string(message);
    urnet_free_string(unsupported);
    urnet_free_string(malformed);
    urnet_free_string(out_of_window);
    CHECK(urnet_release(gaps), "releasing the gap list answered false");
  }

  step("a device with state_store 0, which takes the in-memory store and persists nothing");
  {
    uint64_t volatile_client = urnet_message_loopback_world_client(world);
    char* vs_id = urnet_message_loopback_world_server_id(world);
    REQUIRE(vs_id != NULL, "the world named no server");
    /* PROTOCOL_VERSION IS URNET_MESSAGE_PROTOCOL_VERSION OR 0, AND ANYTHING ELSE IS REFUSED HERE.
     * It used to be accepted whatever it was and refused two calls later, at Hello, by the server;
     * and 0, which every other parameter of this abi reads as "the default", offered no version
     * at all. */
    for (uint32_t bad = 2; bad <= 3; bad += 1) {
      err = NULL;
      uint64_t refused = urnet_message_transport_new(volatile_client, vs_id, bad, 30000, &err);
      CHECK(refused == 0, "protocol_version %u was accepted at transport_new", (unsigned)bad);
      CHECK(err != NULL, "protocol_version %u was refused with no error text", (unsigned)bad);
      if (refused != 0) {
        urnet_message_transport_close(refused);
        urnet_release(refused);
      }
      if (err != NULL) {
        urnet_free_string(err);
        err = NULL;
      }
    }
    err = NULL;
    /* and 0 is the version this build speaks: the in-memory device below connects over it */
    uint64_t vt = urnet_message_transport_new(volatile_client, vs_id, 0, 30000, &err);
    urnet_free_string(vs_id);
    REQUIRE(vt != 0, "the transport for the in-memory device would not open with protocol_version 0");
    char vdir[MAX_PATH + 64];
    REQUIRE(temp_dir(vdir, sizeof(vdir)), "no temp dir");
    err = NULL;
    uint64_t vss = urnet_message_stream_store_open(vdir, &err);
    REQUIRE(vss != 0, "the stream store for the in-memory device would not open");
    uint64_t vr = urnet_message_stream_index_reserver_new(vss);
    REQUIRE(vr != 0, "the reserver for the in-memory device would not open");
    err = NULL;
    /* state_store 0 IS THE POINT. It must take urmessage's in-memory store, not assign a nil
     * pointer into an interface field -- which would be a non-nil interface holding nil and
     * would panic on first use rather than defaulting. */
    uint64_t vd = urnet_message_device_new(vt, vr, 0, 0, 0, NULL, NULL, &err);
    if (vd == 0) {
      show_error("device_new with state_store 0", err);
    }
    CHECK(vd != 0, "a device with state_store 0 was refused; 0 must take the in-memory store");
    if (vd != 0) {
      err = NULL;
      CHECK(urnet_message_device_connect(vd, ctx, &err), "the in-memory device could not connect");
      if (err != NULL) {
        urnet_free_string(err);
        err = NULL;
      }
      /* and it really has no durable store behind it: Restore is refused by name */
      err = NULL;
      uint64_t restored = urnet_message_device_restore(vd, ctx, &err);
      CHECK(restored == 0, "an in-memory device restored %d groups",
            (int)urnet_message_group_list_count(restored));
      CHECK(err != NULL, "an in-memory device answered Restore with no groups AND no error, which "
                         "reads exactly like 'this device was in no groups'");
      if (restored != 0) {
        urnet_release(restored);
      }
      if (err != NULL) {
        urnet_free_string(err);
        err = NULL;
      }
      err = NULL;
      CHECK(urnet_message_device_close(vd, &err), "the in-memory device would not close");
      CHECK(urnet_release(vd), "releasing the in-memory device answered false");
    }
    urnet_message_transport_close(vt);
    CHECK(urnet_release(vt), "releasing the in-memory transport answered false");
    CHECK(urnet_release(vr), "releasing the in-memory reserver answered false");
    err = NULL;
    CHECK(urnet_message_stream_store_close(vss, &err), "the in-memory device's stream store would not close");
    CHECK(urnet_release(vss), "releasing that stream store answered false");
    CHECK(urnet_release(volatile_client), "releasing that client answered false");
  }

  step("a budget SHORTER than one attempt is the bound, with nobody cancelling anything");
  {
    /* THE REVIEW MEASURED THIS BLOCKING FOR 10,000ms: a 500ms budget over the 10s default attempt,
     * because the budget was only consulted after an attempt returned. A ui that passed 500ms to
     * keep the call short got a ten second hang. Here the budget is 500ms, the attempt timeout is
     * left at its default, the server is unroutable, and nothing cancels. */
    attempt_counter counter_d = {0, 0};
    party d = {0};
    char* unrouted_server = urnet_message_loopback_world_server_id(world);
    REQUIRE(party_open(&d, "D", urnet_message_loopback_world_unrouted_client(world), unrouted_server,
                       &counter_d, 500, 0),
            "D would not open");
    urnet_free_string(unrouted_server);
    ULONGLONG bound_started = GetTickCount64();
    err = NULL;
    bool bound_connected = urnet_message_device_connect(d.device, ctx, &err);
    ULONGLONG bound_elapsed = GetTickCount64() - bound_started;
    printf("      a 500ms budget returned after %llums\n", (unsigned long long)bound_elapsed);
    if (err != NULL) {
      printf("      %s\n", err);
      urnet_free_string(err);
      err = NULL;
    }
    CHECK(!bound_connected, "a device with no route to the server reported that it connected");
    CHECK(bound_elapsed < 2000, "a 500ms budget blocked for %llums", (unsigned long long)bound_elapsed);
    CHECK(bound_elapsed >= 400, "a 500ms budget gave up after %llums", (unsigned long long)bound_elapsed);
    party_close(&d);
  }

  step("a blocking call that is cancelled from another thread, instead of waiting its 90s budget");
  attempt_counter counter_c = {0, 0};
  party c = {0};
  char* lost_server = urnet_message_loopback_world_server_id(world);
  /* a 30s budget and an 800ms per-Hello deadline, so that attempts actually FAIL and the
   * progress callback has something to report before the cancel lands. With urmessage's own 10s
   * per-attempt deadline the cancel arrives inside the first Hello and OnAttempt never runs --
   * which is correct (Connect checks the caller's context BEFORE reporting an attempt, because
   * stop means stop) and is not what this case is measuring. */
  REQUIRE(party_open(&c, "C", urnet_message_loopback_world_unrouted_client(world), lost_server,
                     &counter_c, 30000, 800),
          "C would not open");
  urnet_free_string(lost_server);
  uint64_t doomed = urnet_message_context_new();
  REQUIRE(doomed != 0, "context_new answered 0");
  canceller job = {doomed, 2500};
  HANDLE thread = CreateThread(NULL, 0, cancel_after, &job, 0, NULL);
  REQUIRE(thread != NULL, "could not start the cancelling thread");
  ULONGLONG started = GetTickCount64();
  err = NULL;
  bool connected = urnet_message_device_connect(c.device, doomed, &err);
  ULONGLONG elapsed = GetTickCount64() - started;
  WaitForSingleObject(thread, INFINITE);
  CloseHandle(thread);
  printf("      connect returned after %llums\n", (unsigned long long)elapsed);
  if (err != NULL) {
    printf("      %s\n", err);
    urnet_free_string(err);
    err = NULL;
  }
  CHECK(!connected, "a device with no route to the server reported that it connected");
  /* THE BOUND IS THE POINT. urmessage's default budget is 90,000ms. Anything near that means the
   * cancel did nothing and a closing app would hang for a minute and a half. */
  CHECK(elapsed < 10000, "connect took %llums; the cancel did not cut its 90,000ms budget short",
        (unsigned long long)elapsed);
  CHECK(counter_c.calls > 0,
        "the connect-attempt callback never fired, so a caller has no way to say 'Reconnecting...'");
  urnet_message_context_cancel(doomed);
  CHECK(urnet_release(doomed), "releasing the cancelled context answered false");
  party_close(&c);

  step("a PLATFORM-ATTACHED client, built from a credential, with no operator to accept it");
  /* WHAT THIS IS FOR. Until urnet_message_client_new existed, no shipping export produced the
   * `client` handle urnet_message_transport_new takes, so a C caller could reach the in-process
   * loopback world above and NOTHING ELSE. This step is a C caller building the real thing.
   *
   * WHAT IT DOES NOT ASSERT, and it is the honest half: the credential below is an UNSIGNED jwt
   * and the host is example.invalid, which RFC 6761 reserves to never resolve. So the client
   * really is constructed and really does start dialling, and the dial cannot reach anything and
   * would be refused if it did. Nothing here says a frame crossed a platform. What it holds is
   * the SHAPE: every argument that would produce a client nobody can route to is refused by name,
   * a good one answers a handle whose client_id is the credential's and whose url is the one the
   * host and env derive to, and a transport and a device stand up over it. */
  static const char* kJwtWithClientId =
      "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9."
      "eyJjbGllbnRfaWQiOiIwZjlhZDhhMS00ZjNiLTRjMmUtOWI3MS0yYTZkNWM4ZTFmMzAiLCJuZXR3b3JrX25hbWUiOiJjdGVzdCJ9."
      "bm90LWEtc2lnbmF0dXJl";
  static const char* kJwtWithoutClientId =
      "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9."
      "eyJuZXR3b3JrX25hbWUiOiJjdGVzdCJ9."
      "bm90LWEtc2lnbmF0dXJl";
  static const char* kClientIdInTheJwt = "0f9ad8a1-4f3b-4c2e-9b71-2a6d5c8e1f30";

  int64_t before_client = urnet_live_handle_count();
  err = NULL;
  CHECK(urnet_message_client_new(NULL, "example.invalid", NULL, NULL, NULL, &err) == 0,
        "a client with no credential was built anyway");
  CHECK(err != NULL, "a client with no credential was refused with no out_error");
  if (err != NULL) { urnet_free_string(err); err = NULL; }

  /* THE ONE THAT IS NOT OBVIOUS. The jwt parser fills its fields one at a time and SKIPS a claim
   * that is absent, so a token with no client_id answers the ZERO id and no error -- a client
   * that dials, authenticates and is routed nothing, green at every signal. */
  CHECK(urnet_message_client_new(kJwtWithoutClientId, "example.invalid", NULL, NULL, NULL, &err) == 0,
        "a credential naming no client_id built a client at the zero id");
  CHECK(err != NULL, "a credential naming no client_id was refused with no out_error");
  if (err != NULL) { urnet_free_string(err); err = NULL; }

  CHECK(urnet_message_client_new(kJwtWithClientId, "", NULL, NULL, NULL, &err) == 0,
        "a client with nowhere to dial was built anyway");
  CHECK(err != NULL, "a client with no host was refused with no out_error");
  if (err != NULL) { urnet_free_string(err); err = NULL; }

  /* a malformed instance_id is a REFUSAL and not a silently fresh one: a caller that meant to
   * reconnect as a kept installation and mistyped the uuid would otherwise become a new one. */
  CHECK(urnet_message_client_new(kJwtWithClientId, "example.invalid", NULL, "not-a-uuid", NULL, &err) == 0,
        "a malformed instance_id was accepted");
  CHECK(err != NULL, "a malformed instance_id was refused with no out_error");
  if (err != NULL) { urnet_free_string(err); err = NULL; }

  CHECK(urnet_live_handle_count() == before_client,
        "%lld handles were left behind by four refused clients",
        (long long)(urnet_live_handle_count() - before_client));

  uint64_t platform_client =
      urnet_message_client_new(kJwtWithClientId, "example.invalid", "staging", NULL, "ctest", &err);
  if (platform_client == 0) {
    show_error("platform client", err);
    err = NULL;
  }
  REQUIRE(platform_client != 0, "a platform-attached client could not be built");
  char* platform_client_id = urnet_message_client_id(platform_client);
  REQUIRE(platform_client_id != NULL, "the platform client reported no client_id");
  CHECK(strcmp(platform_client_id, kClientIdInTheJwt) == 0,
        "the client dials as %s and the credential names %s", platform_client_id, kClientIdInTheJwt);
  urnet_free_string(platform_client_id);
  char* platform_url = urnet_message_client_platform_url(platform_client);
  REQUIRE(platform_url != NULL, "the platform client reported no url");
  printf("      dialling %s\n", platform_url);
  /* env "staging" prefixes the SERVICE host and not the operator host. A hand-built
   * "wss://connect." + host -- which is what sdk/liveprobe did -- silently dials production. */
  CHECK(strcmp(platform_url, "wss://staging-connect.example.invalid") == 0,
        "the client dialled %s", platform_url);
  urnet_free_string(platform_url);

  /* and the whole stack stands up over it, which is the property the handle registry resolves by:
   * urnet_message_transport_new takes a MessageTransportClient, and this is one. */
  err = NULL;
  char* loop_server = urnet_message_loopback_world_server_id(world);
  REQUIRE(loop_server != NULL, "no server id for the platform-client transport");
  uint64_t platform_transport =
      urnet_message_transport_new(platform_client, loop_server, URNET_MESSAGE_PROTOCOL_VERSION, 1000, &err);
  urnet_free_string(loop_server);
  if (platform_transport == 0) {
    show_error("transport over the platform client", err);
    err = NULL;
  }
  CHECK(platform_transport != 0,
        "a transport would not bind over a platform-attached client, so the client export does not "
        "fit the hole it was built for");
  if (platform_transport != 0) {
    urnet_message_transport_close(platform_transport);
    CHECK(urnet_release(platform_transport), "releasing the platform transport answered false");
  }
  urnet_message_client_close(platform_client);
  /* idempotent, because stop-then-release is not always in that order */
  urnet_message_client_close(platform_client);
  CHECK(urnet_release(platform_client), "releasing the platform client answered false");
  CHECK(urnet_live_handle_count() == before_client,
        "the platform client left %lld handles behind",
        (long long)(urnet_live_handle_count() - before_client));
  CHECK(urnet_message_client_id(0) == NULL, "the zero handle answered a client_id");
  CHECK(urnet_message_client_platform_url(0) == NULL, "the zero handle answered a url");

  step("everything closes, and the handle registry comes back to where it started");
  err = NULL;
  CHECK(urnet_message_group_close(group_a, &err), "A's group would not close");
  CHECK(urnet_release(group_a), "releasing A's group answered false");
  err = NULL;
  CHECK(urnet_message_group_close(group_b, &err), "B's group would not close");
  CHECK(urnet_release(group_b), "releasing B's group answered false");
  urnet_message_context_cancel(ctx);
  CHECK(urnet_release(ctx), "releasing the context answered false");
  party_close(&a);
  party_close(&b);
  urnet_message_loopback_world_close(world);
  CHECK(urnet_release(world), "releasing the world answered false");

  int64_t after = urnet_live_handle_count();
  printf("      %lld live handles, started at %lld\n", (long long)after, (long long)baseline_handles);
  CHECK(after == baseline_handles,
        "%lld handles leaked across one whole conversation (started %lld, ended %lld)",
        (long long)(after - baseline_handles), (long long)baseline_handles, (long long)after);

  /* a released handle must not resolve to anything afterwards, and releasing twice must say so */
  CHECK(!urnet_release(world), "releasing the world a second time answered true");
  CHECK(urnet_message_group_list_count(world) == 0, "a released handle still answered a count");

  report();
  return failures == 0 ? 0 : 1;
}
