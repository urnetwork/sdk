/* HAND-WRITTEN. This is the header for exports_message.go, which is hand-written for the reason
 * stated at the top of that file: the messaging surface lives in sdk/urmessage rather than in
 * package sdk, and gen/gen.go walks package sdk. urnetwork_sdk.h next to this file is generated
 * and carries "DO NOT EDIT"; these declarations are not in it. Both headers ship and they are
 * independent -- include whichever you need, or both.
 *
 * THE MESSAGING C ABI.
 *
 * Contract, the same one urnetwork_sdk.h states, plus what is particular to messaging:
 *
 * - objects are opaque uint64_t handles. release every returned handle with urnet_release
 *   (declared in urnetwork_sdk.h). releasing a handle does not close or stop the object; call
 *   the object's close/cancel function first where one exists. zero is never a handle: it is
 *   "none" going in and "failure" or "empty" coming out.
 * - returned char* strings are owned by the caller: free with urnet_free_string.
 * - functions with a char** out_error set a malloc'd message on failure (free with
 *   urnet_free_string). pass NULL to ignore the text.
 * - the buffer-out pattern for octets: *inout_len is ALWAYS set to the needed size; the copy
 *   happens and true is returned only when out is non-NULL and the capacity passed in was
 *   sufficient. so: call once with out == NULL to size, allocate, call again.
 * - urnet_live_handle_count (urnetwork_sdk.h) counts live handles, for leak checks.
 *
 * A MESSAGE BODY IS OCTETS AND NOT TEXT, AND THE ABI TREATS IT THAT WAY. The seal path takes
 * []byte and the open path hands []byte back; neither is validated as UTF-8 and neither is
 * NUL-free. So a body crosses as (const uint8_t*, int32_t) going in and through the buffer-out
 * pattern coming back, and a body is NEVER a field of a json result -- a char* would truncate at
 * the first 0x00 and a json string would silently replace every invalid byte with U+FFFD. That
 * is why message metadata (urnet_message_list_info) and a message body
 * (urnet_message_list_body) are two calls. Whether your bodies are text is your decision; this
 * abi does not make it, because the content envelope is not ruled.
 *
 * THESE CALLS BLOCK AND YOU SHOULD NOT BE ON A UI THREAD. urnet_message_device_connect,
 * _group_open, _group_send, _group_receive, _group_set_role, _group_transfer_ownership,
 * _group_remove_member,
 * _device_restore and _device_create_group/_join all wait on the network. connect's budget
 * DEFAULTS TO 90 SECONDS, because a reconnecting client is not routed to by the operator for
 * about sixty (measured). run them on a thread of your own and pass a urnet_message_context
 * handle so you can cancel them: urnet_message_context_cancel wakes every call holding that
 * context, from any thread. pass ctx 0 for an uncancellable call.
 *
 * THERE IS NO RECEIVE PUSH. urnet_message_group_receive is a poll and that is what the transport
 * is. nothing arrives on its own.
 *
 * A REAL CONVERSATION IS MORE THAN PLAIN TEXT, AND THE READ SIDE OF IT CROSSES HERE. every
 * message carries the kind it arrived under, a gap reason, the message it replies to, whether a
 * tombstone from its own sender has been applied, and the reactions standing on it. see
 * urnet_message_list_info and the two urnet_message_list_reaction_* calls. THE GAP REASON IS THE
 * ONE TO READ FIRST: a gap is something that IS at that position and cannot be shown, and without
 * it a gap is indistinguishable from a message with no text -- both are body_len 0, and they are
 * different sentences to a user.
 *
 * AND SO DOES THE SEND SIDE OF FOUR OF THEM, WHICH THIS PARAGRAPH USED TO SAY WAS MISSING. it read
 * "WHAT IS NOT HERE: SENDING any of those ... so you can render one and not make one", and that is
 * no longer true: urnet_message_group_send_reply, _react, _unreact and _delete seal a reply, a
 * reaction, an un-reaction and a tombstone, so a client PARTICIPATES in a conversation rather than
 * watching one. each names the message it is about by that message's 32-octet message_id.
 *
 * A message_id CROSSES AS COUNTED OCTETS GOING IN -- the same as group_id and key_package -- and
 * comes BACK as 64 lower case hex characters, in urnet_message_list_info's message_id field. so you
 * decode that hex once into 32 octets and pass those. the two directions differ because metadata
 * crosses as json and a buffer-out call for 32 octets a renderer reads once per row would be a
 * second call per message.
 *
 * THE ROLE MODEL CROSSES TOO (MASTER section 11). every member has a role -- owner, admin, member
 * or observer -- and the library refuses, on both sides, a commit the committer's role does not
 * permit: urnet_message_group_members is the roster with a role per row, _group_my_role is what
 * THIS device may do, _group_set_role and _group_transfer_ownership are the two policy verbs, and
 * _group_remove_member takes one identity out of the group -- every device leaf it holds and its
 * entry in the policy -- in one commit.
 * a verb answers a URNET_MESSAGE_COMMIT_* KIND rather than a bool, because "your role does not
 * permit this", "somebody else's commit landed first, fetch and retry" and "the network failed"
 * are three different things for a caller to do next. see the roster section.
 *
 * AND BEING REMOVED IS A STATE YOU CAN READ, NOT ONLY AN ERROR YOU MIGHT SEE. when a commit takes
 * THIS device out of a group, every later receive, send and commit verb answers by that name -- and
 * urnet_message_group_removal reports it directly, with the last epoch this device was a member of,
 * so a screen can go read-only without having had to make a call fail first. a removed device still
 * reads the conversation up to that epoch; what it cannot do is follow anything above it, or send.
 *
 * WHAT IS STILL NOT HERE: receipts, edit, media, group names, contact discovery, and adding a
 * member to a group that is already open (the go verb exists; its export does not). they are not
 * built underneath this or not exported here, and they are not stubbed. ("removing a member" stood
 * in this list after the verb shipped, two paragraphs under the one that documents it.) TWO
 * PERMANENT STATES ARE STILL ONLY out_error SENTENCES: a group HALTED on a commit this device
 * refused as invalid, and one gone DARK because the epoch key wrap never reached it. both are
 * different screens from a removal, and both are owed the projection the removal now has.
 *
 * SPDX-License-Identifier: MPL-2.0 */
#ifndef URNETWORK_MESSAGE_H
#define URNETWORK_MESSAGE_H

#include <stdint.h>
#include <stdbool.h>

#ifdef __cplusplus
extern "C" {
#endif

/* ----- constants ----- */

/* the one message server protocol version this build speaks, which is what
 * urnet_message_transport_new's protocol_version takes. 0 takes it too, as every other 0 in this
 * abi takes a default; any other value is refused at transport_new, by name. */
#define URNET_MESSAGE_PROTOCOL_VERSION 1

/* the content kinds this build knows, which is what urnet_message_list_info's "kind" carries.
 * THE CODE IS THE VERSION OF ITS OWN GRAMMAR: a later kind is a NEW code and never a flag inside an
 * old one, so a build that does not know a code keeps the record's position and shows a
 * placeholder. TREAT ANY OTHER VALUE AS EXACTLY THAT -- do not refuse it, and do not assume the
 * list below is closed.
 *
 * FOUR OF THESE NEVER APPEAR AS A MESSAGE YOU RENDER. a REACTION_ADD, a REACTION_REMOVE and a
 * TOMBSTONE change ANOTHER message and add no line of their own -- they arrive as the "reactions"
 * and "deleted" of the message they name -- and a COVER is traffic that exists to look like a
 * message and is discarded. they are listed because urnet_message_group_send answers the metadata
 * of the record it just sealed, and because a GAP carries the code it ARRIVED under. */
#define URNET_MESSAGE_KIND_TEXT            0x01
#define URNET_MESSAGE_KIND_REPLY           0x02
#define URNET_MESSAGE_KIND_ATTACHMENT      0x03
#define URNET_MESSAGE_KIND_TOMBSTONE       0x04
#define URNET_MESSAGE_KIND_REACTION_ADD    0x05
#define URNET_MESSAGE_KIND_REACTION_REMOVE 0x06
#define URNET_MESSAGE_KIND_COVER           0x07

/* the values urnet_message_list_info's "gap" takes, as strings, and "" for a message that is a
 * message. spec A section 7.4's set is closed at seven and THIS BUILD PRODUCES THREE; the other
 * four are waiting on machinery that does not exist here, so a caller that shows a default for an
 * unrecognised reason is right rather than lazy.
 *
 * THE DISTINCTION BETWEEN THE FIRST TWO IS LOAD BEARING IN BOTH DIRECTIONS and the copy differs:
 * MALFORMED is a fault and NO upgrade fixes it, so it must not offer one; UNSUPPORTED is a member
 * running a newer build and the upgrade is the whole answer. showing either sentence for the other
 * either accuses a correct sender or sends a user after an upgrade that cannot help.
 *
 * OUT_OF_WINDOW IS NEITHER AND IS NOBODY'S FAULT: the record was sealed at an epoch no key
 * schedule on this device reaches -- more than the past epoch window behind, or before this device
 * was admitted -- so it never opened. it is the one reason this build produces for which
 * sender_role_at_send is "", because a device that cannot obtain an epoch cannot say who held
 * which role in it. it was undeclared here while the library produced it (a later joiner produces
 * one per pre-admission record), which left a C caller reading the product's commonest gap as an
 * unrecognised string. */
#define URNET_MESSAGE_GAP_MALFORMED     "malformed"
#define URNET_MESSAGE_GAP_UNSUPPORTED   "unsupported"
#define URNET_MESSAGE_GAP_OUT_OF_WINDOW "out_of_window"

/* what urnet_message_group_set_role, urnet_message_group_transfer_ownership and
 * urnet_message_group_remove_member answer. out_error is set on everything but OK. BRANCH ON THE
 * KIND AND SHOW THE TEXT: the kind is what to do next and the text is why.
 *
 * REFUSED: this device's role does not permit the change (MASTER section 11). nothing was built,
 * nothing moved for anybody, the stats' commit_refused_own moved by one, and retrying answers the
 * same. out_error carries the rule's own sentence.
 * LOST: another member's commit closed this epoch first. the group is exactly where it was; call
 * urnet_message_group_receive to follow the winner, then the same verb again. it is not an error
 * to show: the change may still be right.
 * INVALID: the request was malformed and refused by name before any rule was reached -- a role
 * that is not "admin", "member" or "observer", a set_role naming the owner's own identity (both
 * because ownership moves through transfer_ownership), a transfer to the identity that already
 * owns the group, a remove_member naming the identity that OWNS the group or THIS device's own
 * identity or an identity no leaf carries, an identity_pub_hex that is not hex. nothing counted.
 * it is a caller bug, and the text names the door: transfer ownership first, or -- for your own
 * identity -- ask an admin, because no identity's last leaf ever leaves in its own commit.
 * FAILED: everything else -- the transport, a group that is not open or not yet reconciled, a
 * closed handle. FAILED with out_error left NULL is an unknown self or ctx handle, which this abi
 * logs by name rather than reporting.
 *
 * a go test holds these equal, by name and value, to the library's own constants. */
#define URNET_MESSAGE_COMMIT_OK      0
#define URNET_MESSAGE_COMMIT_REFUSED 1
#define URNET_MESSAGE_COMMIT_LOST    2
#define URNET_MESSAGE_COMMIT_INVALID 3
#define URNET_MESSAGE_COMMIT_FAILED  4

/* ----- callback types ----- */

/* one Hello that did not connect. fires on the thread inside urnet_message_device_connect --
 * this is a progress report from a blocking call, not an async completion. it must not call back
 * into the device it came from. err is only valid during the call.
 *
 * the typedef is repeated here rather than included from callbacks_message.h so that this header
 * stands alone, which is what urnetwork_sdk.h does with the generated callbacks.h. */
#ifndef URNETWORK_MESSAGE_CALLBACKS_H
typedef void (*urnet_message_connect_attempt_cb)(void* user_data, int32_t attempt, int64_t elapsed_ms, int64_t backoff_ms, const char* err);
#endif

/* ----- cancellation ----- */

/* a context a blocking call can be woken from. release with urnet_release AFTER cancelling. */
uint64_t urnet_message_context_new(void);
/* wake every call holding this context. safe from any thread, idempotent. */
void urnet_message_context_cancel(uint64_t self);

/* ----- the durable stores ----- */

/* the stream index allocator's backing store. one directory per device, held under a
 * single-writer exclusion, fsync'd before an index is handed out -- a reused stream index is a
 * reused nonce under a reused record key. */
uint64_t urnet_message_stream_store_open(const char* dir, char** out_error);
bool urnet_message_stream_store_close(uint64_t self, char** out_error);
uint64_t urnet_message_stream_index_reserver_new(uint64_t stream_store);

/* where MLS keeps group state and private keys, so that a restart is a restore. EVERY OCTET IS
 * WRITTEN IN THE CLEAR and the only thing protecting it is file permissions, which is a real
 * bound on POSIX and is not a bound this code sets on Windows. a COPY of this directory is a
 * second device on one identity; see urnet_message_device_restore. */
uint64_t urnet_message_durable_state_store_open(const char* dir, char** out_error);
bool urnet_message_durable_state_store_close(uint64_t self, char** out_error);

/* ----- the platform-attached client ----- */

/* build a connect client ATTACHED TO THE PLATFORM and dial it: this is the `client` handle
 * urnet_message_transport_new takes, and it is what lets this abi reach a message server that is
 * not in your own process.
 *
 * by_client_jwt is an operator-minted ByJwt for a network_client. NOTHING HERE MINTS ONE: that is
 * an admin action against a running URnetwork operator, it is the half of S2-7 that is still open,
 * and this call takes the credential you already hold. IT IS A SECRET -- do not log it.
 *
 * host is the operator host name, e.g. "ur.io". the platform and api urls are DERIVED from it:
 * env "" or "main" gives wss://connect.<host>, any other env gives wss://<env>-connect.<host>.
 * env, instance_id and app_version may each be NULL. instance_id is a uuid identifying THIS
 * installation -- pass NULL or "" to draw a fresh one, or the uuid you kept to reconnect as the
 * same one; a malformed uuid is refused rather than silently replaced. every refusal answers 0
 * AND sets out_error.
 *
 * IT DOES NOT BLOCK AND IT DOES NOT TELL YOU WHETHER THE CREDENTIAL WAS ACCEPTED. the dial runs on
 * its own thread and reconnects by itself; urnet_message_device_connect is what finds out.
 *
 * THIS PATH HAS NOT BEEN RUN AGAINST A REAL OPERATOR FROM THIS ABI. what is under test is the
 * shape -- refusals, the derived url, the client_id, the provide modes -- and not that a frame
 * crossed.
 *
 * CLOSE IT WITH urnet_message_client_close BEFORE urnet_release. release alone leaves the
 * websocket and its reconnect loop running. close the device and the transport first: they are
 * built over this and neither closes it. */
uint64_t urnet_message_client_new(const char* by_client_jwt, const char* host, const char* env, const char* instance_id, const char* app_version, char** out_error);
/* the client_id the credential names, as a uuid string, which is the identity the platform routes
 * to. free with urnet_free_string. */
char* urnet_message_client_id(uint64_t self);
/* the url this client actually dialled. the derivation from host and env happens inside the
 * library, and dialling the production authority from a staging env looks exactly like working.
 * free with urnet_free_string. */
char* urnet_message_client_platform_url(uint64_t self);
/* stop the platform transport, the client and everything under them. idempotent. */
void urnet_message_client_close(uint64_t self);

/* ----- push (spec B 4.3.5) ----- */

/* subscribe this group to the server's push when it holds no current subscription: none yet, one
 * at an older epoch, or one on a connection a Hello has since replaced. answers 1 when it
 * subscribed now, 0 when it was already current, -1 with out_error when the server refused. after
 * a 1, receive once: a subscription only announces what arrives after it. */
int32_t urnet_message_group_ensure_subscribed(uint64_t self, uint64_t ctx, char** out_error);
/* wait up to timeout_ms for a push and answer the group id it names, as hex, or NULL on timeout.
 * a push carries no records: answer it with urnet_message_group_receive. free with
 * urnet_free_string. */
char* urnet_message_device_wait_push(uint64_t self, int64_t timeout_ms);

/* ----- the route client: the server's own endpoint ----- */

/* the two ways a route reaches the server's own endpoint */
#define URNET_MESSAGE_ROUTE_URNETWORK 0
#define URNET_MESSAGE_ROUTE_DIRECT 1

/* reach a message server at its OWN TLS endpoint rather than through the operator. the handle is a
 * client urnet_message_transport_new takes, exactly as it takes urnet_message_client_new's.
 *
 * mode URNET_MESSAGE_ROUTE_URNETWORK (the default) leaves through a URnetwork exit provider over a
 * tunnel inside this process -- no adapter, no service, no administrator -- and the server sees the
 * exit's address, not this device's. URNET_MESSAGE_ROUTE_DIRECT is an ordinary connection and shows
 * this device's address to the message server.
 *
 * endpoint is a wss:// url. pin is the SHA-256 of the server certificate's SubjectPublicKeyInfo, 64
 * hex characters, optionally "sha256/"-prefixed: a server presenting any other key is never sent a
 * frame. by_client_jwt (A SECRET) and host are what the tunnel's exit clients are minted from, and
 * are ignored for DIRECT. env and app_version may be NULL. every refusal answers 0 AND sets
 * out_error. it does not block; urnet_message_device_connect finds out whether the server answered.
 *
 * CLOSE IT WITH urnet_message_route_client_close BEFORE urnet_release, after the device and the
 * transport built over it. */
uint64_t urnet_message_route_client_new(const char* by_client_jwt, const char* host, const char* env, const char* endpoint, const char* pin, int32_t mode, const char* app_version, char** out_error);
/* the route's state as json: mode, connected, connects (above 1 means it re-established),
 * last_error, and in URNETWORK mode window_providers and window_countries, the exits the tunnel
 * holds. free with urnet_free_string. */
char* urnet_message_route_client_status(uint64_t self);
/* end the session and the tunnel. idempotent. */
void urnet_message_route_client_close(uint64_t self);

/* ----- the transport ----- */

/* bind to one message server over a connect client YOU own: nothing here dials, authenticates or
 * closes it. server_client_id is a uuid string. protocol_version is URNET_MESSAGE_PROTOCOL_VERSION,
 * or 0 for it; any other value answers 0 and out_error here, rather than a Hello the server refuses
 * later. timeout_ms 0 takes the binding's default.
 *
 * WHERE THE client HANDLE COMES FROM: urnet_message_client_new, above -- a connect.Client receives
 * a frame only through an in-process route or through a platform transport dialling an operator
 * with a minted ByJwt, and that export is the second. what is still open of S2-7 is the CREDENTIAL
 * and only the credential. */
uint64_t urnet_message_transport_new(uint64_t client, const char* server_client_id, uint32_t protocol_version, int64_t timeout_ms, char** out_error);
/* stop receiving. the connect client under it is yours and is NOT closed. */
void urnet_message_transport_close(uint64_t self);

/* ----- the device ----- */

/* state_store may be 0, which takes an IN-MEMORY store: it persists nothing and every group is
 * gone when the process ends. connect_budget_ms and connect_attempt_timeout_ms 0 take 90s and
 * 10s. THE BUDGET BOUNDS HOW LONG urnet_message_device_connect BLOCKS whatever the attempt timeout
 * says: every attempt is cut to what is left of it, so a 500ms budget returns at about 500ms even
 * with the 10s default attempt. connect_attempt_cb may be NULL; when it is not it fires for every Hello that did not
 * connect, on the thread inside urnet_message_device_connect -- it is how you say
 * "Reconnecting..." DURING the window rather than after it. */
uint64_t urnet_message_device_new(uint64_t transport, uint64_t reserver, uint64_t state_store, int64_t connect_budget_ms, int64_t connect_attempt_timeout_ms, urnet_message_connect_attempt_cb connect_attempt_cb, void* connect_attempt_user_data, char** out_error);
/* say Hello. BLOCKS for up to the budget, and not an attempt past it. a budget spent on silence is "not yet, ask again" and
 * is NOT a failure: on the deployed server a reconnecting client_id is not routed to for about
 * sixty seconds. showing a user "could not connect" here tells them something false. */
bool urnet_message_device_connect(uint64_t self, uint64_t ctx, char** out_error);
bool urnet_message_device_close(uint64_t self, char** out_error);
/* buffer-out. the key package another device's urnet_message_group_add_member takes. */
bool urnet_message_device_key_package(uint64_t self, uint8_t* out, int32_t* inout_len, char** out_error);
/* a group list handle, or 0 when there are none. */
uint64_t urnet_message_device_groups(uint64_t self);
/* rebuild every group a DURABLE state store holds. a restored group will not seal until
 * urnet_message_group_receive has run once over it. a non-zero list and a non-NULL out_error can
 * both come back. */
uint64_t urnet_message_device_restore(uint64_t self, uint64_t ctx, char** out_error);
/* group_id is 32 octets. */
uint64_t urnet_message_device_create_group(uint64_t self, uint64_t ctx, const uint8_t* group_id, int32_t group_id_len, char** out_error);
/* a group joined ABOVE EPOCH ONE will not seal until urnet_message_group_receive has run once over
 * it -- that is urmessage's ErrStreamFloorUnheld, and it is what bounds a leaf a removed member may
 * have stood at: the joiner inherits that member's sender_handle byte for byte, and a first send
 * with no receive behind it collides with a stream claim the server already holds and is then
 * refused FOR THE LIFE OF THE PROCESS. receive once, then send: a group joined at epoch one never
 * carries it, so that order is correct in both cases and needs no epoch test. errors here are
 * sentences and not codes -- this abi has no typed error channel. */
uint64_t urnet_message_device_join(uint64_t self, uint64_t ctx, uint64_t invite, char** out_error);

/* ----- the invite, which is secret in full ----- */

/* the alpha adds exactly ONE member, BEFORE urnet_message_group_open: it is the commit that
 * opens epoch 1. a second add is a second epoch and is refused by name. */
uint64_t urnet_message_group_add_member(uint64_t self, const uint8_t* key_package, int32_t key_package_len, char** out_error);
/* ADDS SOMEBODY TO A GROUP THAT IS ALREADY OPEN, which the call above cannot: that one builds the
 * founding commit and is refused once the group is open. This one stages a commit at the current
 * epoch, publishes it, and merges only on the server's REASON_OK, so a refused or lost commit
 * leaves the group where it was. ROLE-GATED ON THE SENDING SIDE: adding a new identity is an
 * admin's or the owner's (MASTER section 11), and an identity's own second device is its own to
 * add at any role. Answers 0 and out_error on a refusal, a lost epoch race or a transport failure;
 * the sentence carries urmessage's own stable prefix, which is what a caller branches on -- there
 * is no commit-kind here, because the return value is spent on the invite handle you need to
 * encode an invitation with. */
uint64_t urnet_message_group_add_member_and_publish(uint64_t self, uint64_t ctx, const uint8_t* key_package, int32_t key_package_len, char** out_error);
/* buffer-out. WHAT COMES OUT IS KEY MATERIAL: an invite that reaches a third party is a group
 * that third party is in. move it like a private key and destroy it afterwards. */
bool urnet_message_invite_encode(uint64_t self, uint8_t* out, int32_t* inout_len, char** out_error);
/* an invite ends with a checksum of everything before it, so a damaged one -- truncated, a mangled
 * paste, one octet rewritten -- answers 0 and out_error HERE, where a user pasting it can be told,
 * and never joins. the checksum is not an authentication: move the invite over a channel that is
 * already authenticated. */
uint64_t urnet_message_parse_invite(const uint8_t* encoded, int32_t encoded_len, char** out_error);

/* ----- the group ----- */

/* publish the group on the server: the founding commit, the epoch's wraps, the marker that
 * closes them. comes AFTER add_member. */
bool urnet_message_group_open(uint64_t self, uint64_t ctx, char** out_error);
/* seal one body and submit it. the body is counted octets and crosses byte for byte. the result
 * is this message's metadata as json WITHOUT the body -- you already have the body -- or NULL on
 * failure. free with urnet_free_string. */
char* urnet_message_group_send(uint64_t self, uint64_t ctx, const uint8_t* body, int32_t body_len, char** out_error);

/* THE FOUR VERBS THAT NAME ANOTHER MESSAGE. each blocks on its submit and takes a cancel handle,
 * exactly as urnet_message_group_send does, and each answers that record's own metadata as json --
 * the same shape urnet_message_group_send answers -- or NULL with out_error set. free with
 * urnet_free_string.
 *
 * message_id IS 32 OCTETS AND YOU PASS THE LENGTH. you get it as the 64 hex characters in
 * urnet_message_list_info's message_id field: decode once, pass the octets. any other width is
 * refused by name before anything is sealed.
 *
 * WHAT COMES BACK FROM THE LAST THREE IS NOT A LINE OF THE CONVERSATION. a reaction, an
 * un-reaction and a tombstone CHANGE another message and add no entry of their own, so the value
 * exists to give you the record_id and message_id of what you just sent -- what a later unreact,
 * and any log, would need. do not append it to a view. the change itself appears on the TARGET,
 * on the next urnet_message_group_messages. */

/* a reply carries its parent's NAME and never its text: it renders by looking the parent up. the
 * parent is NOT required to be present -- it may be deleted, pruned or not yet fetched -- which is
 * the one way this differs from the three below. the body is counted octets, as _send's is. */
char* urnet_message_group_send_reply(uint64_t self, uint64_t ctx, const uint8_t* reply_to, int32_t reply_to_len, const uint8_t* body, int32_t body_len, char** out_error);
/* react to a message this device holds. the emoji is a NUL-terminated utf-8 string and is checked
 * as valid utf-8 of 1..64 octets before anything is sealed; it is NOT checked to be exactly one
 * grapheme cluster, so two characters reach every member as two characters. a target this device
 * does not hold, a target that is a GAP, and a target that is itself a reaction, a tombstone or a
 * cover are all refused here and seal nothing. */
char* urnet_message_group_react(uint64_t self, uint64_t ctx, const uint8_t* target, int32_t target_len, const char* emoji, char** out_error);
/* take back a reaction: it cancels an ADD with the same (reactor, target, emoji) and NOBODY ELSE'S
 * -- the reactor is a sender_handle, so a second device of one person cannot take back the first's.
 * it is a RECORD and not an undo; the add stays on the server and every member replays both. */
char* urnet_message_group_unreact(uint64_t self, uint64_t ctx, const uint8_t* target, int32_t target_len, const char* emoji, char** out_error);
/* delete a message of THIS DEVICE'S OWN. a tombstone over anybody else's is refused here and would
 * be ignored by every honest receiver anyway. IT DOES NOT ERASE THE RECORD ON THE SERVER AND IT
 * DOES NOT CLEAR THE TEXT: the target keeps its body and its body_len and `deleted` goes true
 * beside them. what a deleted line looks like is your decision; this abi refuses to make it. */
char* urnet_message_group_delete(uint64_t self, uint64_t ctx, const uint8_t* target, int32_t target_len, char** out_error);

/* fetch. returns a message list handle, or 0 when nothing new arrived.
 *
 * A NON-ZERO RESULT AND A NON-NULL out_error CAN BOTH COME BACK, and code that reads an error as
 * "nothing arrived" will drop real messages: a page bound reached with more to come, a server
 * that named a high water above what it handed over, and a record given up on after every retry
 * are all answers that carry messages AND a reason.
 *
 * 0 with no error is also what an unknown self or ctx handle answers -- a handle that does not
 * resolve is a programming error, logged by name rather than returned as out_error, which is
 * this abi's convention everywhere. It is worth knowing here because "nothing new" is the
 * common answer this one shares with it. */
uint64_t urnet_message_group_receive(uint64_t self, uint64_t ctx, char** out_error);
/* every message this group has sent or received, in the order it learned them. */
uint64_t urnet_message_group_messages(uint64_t self);
/* buffer-out, 32 octets. */
bool urnet_message_group_id(uint64_t self, uint8_t* out, int32_t* inout_len);
uint64_t urnet_message_group_epoch(uint64_t self);
bool urnet_message_group_is_open(uint64_t self);
/* what this group has SEEN, as json: fetched, opened, skipped_ceremony, skipped_own, opened_own,
 * own_without_copy, skipped_seen, unopened, omitted, skipped_class, wrap_opened, wrap_missing,
 * wrap_unreadable, wrap_orphaned, gap_malformed, gap_unsupported,
 * gap_out_of_window, opened_past_epoch, hidden_observer, observer_reaction_refused,
 * role_undeterminable, ingested,
 * commit_refused, commit_refused_own, failed_open, submitted, rebound, pages, unattested,
 * stream_floor_seeded, unopened_unattributed.
 *
 * THE LIST ABOVE IS THE JSON'S OWN KEY LIST, IN ITS ORDER, and a go test in this directory reads it
 * off this file and holds it equal to the keys the json carries -- it went stale once, omitting
 * four counters with every test green, and a documented list nothing checks is a list a C caller
 * trusts for nothing. gap_out_of_window counts records sealed at an epoch no schedule on this
 * device reaches; opened_past_epoch counts records opened under a prior epoch's schedule because
 * this device was a member then. hidden_observer counts lines whose sender was an OBSERVER at the
 * epoch it sealed them -- a member running a build that does not take the send refusal, since
 * OBSERVER is enforced in the client and not at the server -- and the rows are in the log with
 * their bodies intact, collapsed by sender_role_at_send rather than dropped.
 * observer_reaction_refused counts the other answer, and the asymmetry is the rule: an observer's
 * REACTION is NOT APPLIED at all, so it never appears in any message's reactions array and there is
 * nothing to collapse -- a message is kept because dropping it would hide that something was said,
 * and a reaction that is not applied hides nothing, since the message it names is right there whole.
 * It is one per record. An observer's TOMBSTONE is applied and counted by neither: it only ever
 * retracts that observer's own message. role_undeterminable
 * counts records that opened and whose sender's role could not be read: it MUST STAY ZERO, because
 * the role is read off the same handle the open read, and it is not gap_out_of_window's
 * counterpart -- a record no schedule reaches never opens and is never asked about.
 * wrap_opened, wrap_missing, wrap_unreadable and wrap_orphaned are the epoch device wrap that
 * carries this group post-quantum secret for the epoch a commit opens (ledger item 251).
 * wrap_opened rises by one per epoch change this device did not commit itself, and a zero
 * across a commit is the first thing to look at. The other three are FAILURES with a typed
 * error each, and they are three numbers rather than one because they have three repairs: a
 * wrap that never arrived is a committer that left this device out of the fan-out; one that did
 * not open was sealed to a key this device does not hold; and wrap_orphaned is the fan-out of a
 * committer that LOST its race to open the epoch, which repairs itself -- a number there with
 * no wrap_missing beside it is the healthy reading. A device with wrap_missing or
 * wrap_unreadable above zero can neither read nor write at that epoch and says so by name,
 * rather than meeting an undiagnosable refusal from the server.
 * ingested counts membership-change commits this device followed
 * into the next epoch; commit_refused counts the ones its receiving-side role check refused --
 * a number there is a member that committed what its role does not permit, and a group this
 * device can no longer write to until it is re-founded. commit_refused_own counts the commits
 * THIS device was asked to make and refused before building them, by the same rules: nothing
 * moved for anybody, and the go verb that asked answered the refusal as its error (the exports
 * over those verbs are a later step).
 *
 * gap_malformed AND gap_unsupported ARE THE TWO YOU WATCH FOR A RECORD THAT COULD NOT BE READ, and
 * they are counters rather than an error because a permanent post-open refusal no longer fails:
 * the record resolves once, failed_open does not move, unopened does not move, and
 * urnet_message_group_receive answers no out_error. code that watches only out_error will not
 * learn that a line is missing. gap_unsupported growing is this build getting old; gap_malformed
 * growing is a fault. opened_own counts this device's own records shown from the copy it
 * persisted when it sent them -- a member cannot decrypt its own records -- and own_without_copy
 * counts its own records it has no copy of and cannot show. it exists so that "nothing arrived" and "something arrived and this build would
 * not open it" are two readings rather than one silence. free with urnet_free_string. */
char* urnet_message_group_stats(uint64_t self);
/* WHETHER A COMMIT HAS TAKEN THIS DEVICE OUT OF THIS GROUP, as json:
 *   {"removed":true,"removed_epoch":7}
 *
 * THE SHAPE ABOVE IS THE JSON'S OWN KEY LIST, IN ITS ORDER, held by a go test in this directory in
 * the same way the message and member shapes are. NULL for an unknown handle, which is this abi's
 * convention; free with urnet_free_string.
 *
 * `removed` FALSE IS THE ORDINARY ANSWER and says nothing else about the group's health. TRUE means a
 * VALID commit this group received removed this device: it is not the same thing as a commit this
 * device REFUSED (that group is halted and is still a member) and not the same as a wrap that never
 * arrived (that group followed the commit and holds no keys for the epoch). those two are still only
 * out_error sentences here.
 *
 * WHAT A UI OWES IT. it never clears -- the only way back into the group is to be added again, which
 * arrives as a new invite and a different group -- so the right shape is a permanent read-only state
 * and not a retry: disable the composer with an inline reason (Spec C section 5's disabled-composer
 * rule, and screen 10's read-only variant), and keep the transcript. `removed_epoch` is the LAST epoch
 * this device was a member of, and the server serves it nothing above that epoch, so it is exactly
 * where the conversation this device can still read stops.
 *
 * READ IT AFTER ANY receive OR send THAT FAILED, and on restore: it is persisted, so a device that was
 * removed while the app was closed comes back already knowing, and its first receive does not have to
 * fail for the screen to be right. */
char* urnet_message_group_removal(uint64_t self);
bool urnet_message_group_close(uint64_t self, char** out_error);

/* ----- the roster and the two role verbs (MASTER section 11) ----- */

/* the roster: every member of this group at its current epoch, in leaf order, each with the role
 * the live policy gives it, as a member list handle (urnet_message_member_list_count / _info). it
 * is what a roster screen shows, and it is exactly what this device would be judged by were it to
 * commit now. 0 with out_error set when it cannot be read (a closed group); 0 with no error for an
 * unknown handle. a group always holds at least this device's own leaf, so a non-zero handle is
 * never empty. re-read it after every urnet_message_group_receive that moved the epoch: a commit
 * from another member may have changed a role. */
uint64_t urnet_message_group_members(uint64_t self, char** out_error);
/* "owner", "admin", "member" or "observer": what THIS device may do, read at its own leaf. an
 * identity the policy does not name is "member". NULL with out_error set when it cannot be read.
 * free with urnet_free_string. */
char* urnet_message_group_my_role(uint64_t self, char** out_error);
/* make one identity an "admin", a "member" or an "observer", in one commit. BLOCKS on the submit
 * and takes a cancel handle. identity_pub_hex is the identity_pub a member info carries, passed
 * back as is. answers a URNET_MESSAGE_COMMIT_* kind: who may set what is section 11's table --
 * the owner may change who is an admin, an admin may set member/observer -- and a caller that is
 * neither admin nor owner is REFUSED whatever it asked for. "owner" is INVALID here: ownership
 * moves through the call below. naming the role the identity already holds is OK and moves
 * nothing, so there is nothing to fetch afterwards. */
int32_t urnet_message_group_set_role(uint64_t self, uint64_t ctx, const char* identity_pub_hex, const char* role, char** out_error);
/* make one identity the OWNER; this device, the outgoing owner, becomes an ADMIN in the same
 * commit. BLOCKS on the submit and takes a cancel handle. the new owner must already be a member:
 * a stranger is REFUSED, the current owner is INVALID, and anybody but the owner calling this is
 * REFUSED. answers a URNET_MESSAGE_COMMIT_* kind. */
int32_t urnet_message_group_transfer_ownership(uint64_t self, uint64_t ctx, const char* identity_pub_hex, char** out_error);
/* take one identity out of the group: EVERY device leaf it holds and its entry in the group's
 * policy, in ONE commit. BLOCKS on the submit and takes a cancel handle. identity_pub_hex is the
 * identity_pub a member info carries, so a roster row is all a caller needs -- one call per PERSON
 * and not one per device, because a removal that left one of somebody's devices in the group would
 * have removed nobody. answers a URNET_MESSAGE_COMMIT_* kind: a member or an observer is REFUSED,
 * and only the owner may remove an admin. the OWNER's identity is INVALID -- transfer ownership
 * first, and the outgoing owner is then an admin the new owner may remove -- and so is THIS
 * device's own identity, which is a leave and not a removal. after it lands, the removed identity
 * can neither read the group nor write to it: the epoch the removal opens runs on a fresh secret
 * that is delivered to every other member and not to it. re-read the roster afterwards. */
int32_t urnet_message_group_remove_member(uint64_t self, uint64_t ctx, const char* identity_pub_hex, char** out_error);

/* ----- the list handles ----- */

/* a list of groups or messages is ONE handle with indexed accessors, not N handles and not one
 * json blob (a blob cannot carry a body). an empty list is handle 0, and every accessor answers
 * 0/NULL/false on handle 0 rather than failing. */

int32_t urnet_message_group_list_count(uint64_t self);
/* a NEW handle onto the group at index, which you release. two calls at one index are two
 * handles onto one group. */
uint64_t urnet_message_group_list_at(uint64_t self, int32_t index);

int32_t urnet_message_list_count(uint64_t self);
/* one message's metadata as json, WITHOUT the body and WITHOUT its reactions:
 *   {"record_id":u64,"sender_handle":"<32 hex>","sender_identity":"<hex>","mine":bool,
 *    "sender_role_at_send":"member",
 *    "sent_at_ms":i64,"body_len":i32,"message_id":"<64 hex>","kind":u8,"gap":"","reply_to_id":"",
 *    "deleted":bool,"reaction_count":i32}
 * THE KEY LIST ABOVE IS THE JSON'S OWN, IN ITS ORDER, and a go test in this directory holds it so.
 * sender_handle is 16 opaque octets and IS NOT A NAME and IS NOT AN ATTRIBUTION EITHER: it is
 * derived from the LEAF alone and the group's handle key never rotates, so a member added onto a
 * REMOVED member's leaf carries the removed member's sender_handle byte for byte -- two people,
 * one label, for ever (msgrepo ledger item 245). JOIN A LINE TO A ROSTER ROW ON sender_identity,
 * which is what MLS signs and is the same value urnet_message_group_members answers as
 * identity_pub; it is "" only on a record that did not open and that this device did not seal.
 *
 * mine is decided ON sender_identity OR ON OCTETS THIS DEVICE PRODUCED, and NEVER on
 * sender_handle. a record that opened is mine when its sender_identity is this device's own; a
 * record that did NOT open is mine only when this device sealed at that stream index and the
 * record carries the body_hash it sealed there -- and such a record carries this device's
 * sender_identity too. so mine and sender_identity agree on every line, and a caller that keys
 * its rows on sender_identity may read mine beside them. a build that took mine off
 * sender_handle showed a REMOVED member's whole history as this device's own the day this device
 * landed on that member's leaf.
 *
 * sender_role_at_send is the role the SENDER HELD AT THE EPOCH THIS RECORD WAS SEALED AT --
 * "owner", "admin", "member", "observer" -- and "" on a record that did not open. IT IS A FACT
 * ABOUT AN EPOCH AND NOT ABOUT NOW: urnet_message_group_members answers the roles the group has
 * today, and a line written before a demotion was written under the role its sender held then, so
 * joining a row to the roster on sender_handle and reading the role off that row relabels history
 * at every role change. "observer" IS THE ONE VALUE THAT ASKS FOR ANYTHING: collapse the row to
 * the system line "A message from an observer was hidden.", with the content one expansion away.
 * THE RECORD IS STILL HERE AND ITS BODY IS INTACT -- body_len is the real length and
 * urnet_message_list_body still hands it back -- because a row dropped is indistinguishable from a
 * record that never arrived. it is not a gap: gap stays "".
 *
 * message_id is 32 octets and IS the name to quote: a reply, a reaction, a tombstone or a read
 * cursor has to say which message it is about, and record_id cannot -- record_id is the SERVER's
 * per-group counter, so it is zero on a message whose submit response was lost. it is a NAME and
 * not an authentication: the key it is derived under is group-shared, so any member can compute
 * any member's id at any position. what makes an id trustworthy is that the record it names
 * opened.
 *
 * gap IS THE FIELD TO BRANCH ON FIRST, and "" is the answer on a message that is a message. a
 * non-empty gap means something IS at this position in the conversation and this build cannot show
 * it: the record kept its place and its message_id, body_len is 0, and one closed placeholder is
 * what to draw -- see URNET_MESSAGE_GAP_*. DO NOT BRANCH ON kind FOR THIS: on a gap, kind is the
 * code the record ARRIVED under and not what the record is, so a malformed REPLY carries
 * URNET_MESSAGE_KIND_REPLY and is still a gap.
 *
 * reply_to_id is the parent's message_id on a REPLY and "" on everything else. THE QUOTED TEXT
 * NEVER TRAVELS: look the parent up, and be ready for it to be missing -- deleted, pruned, or not
 * fetched by this device yet.
 *
 * deleted means a tombstone FROM THIS MESSAGE'S OWN SENDER has been applied. the body is still
 * here and urnet_message_list_body still hands it back: the library refuses to decide what a UI
 * does with a deleted line, and the record is on the server either way.
 *
 * reaction_count is the bound on urnet_message_list_reaction_info's reaction_index, carried here
 * for the same reason body_len is -- one info string per row, and the common answer is 0.
 * free with urnet_free_string. */
char* urnet_message_list_info(uint64_t self, int32_t index);
/* one message's body, byte for byte, through the buffer-out pattern. the ONLY way a body leaves
 * this abi. */
bool urnet_message_list_body(uint64_t self, int32_t index, uint8_t* out, int32_t* inout_len);

/* the reactions standing on the message at index: a count and an accessor, which is this abi's
 * shape for a collection one level down. they are NOT an array inside the info json, and the
 * reason is a bound: NOTHING CAPS HOW MANY REACTIONS ONE MESSAGE CAN CARRY, so an inlined array
 * would make one row's metadata a string whose size another member chose. with these two you
 * render the first few and pay for what you asked for.
 *
 * BOTH ANSWER FROM THE INSTANT THE LIST HANDLE WAS MADE, so `for (k = 0; k < count; k++)` cannot
 * be overtaken by a reaction landing under a urnet_message_group_receive on another thread. call
 * urnet_message_group_messages again to see later ones.
 *
 * one reaction is {"sender_handle":"<32 hex>","emoji":"...","mine":bool}. the reactor is a
 * sender_handle and NOT a person -- two devices of one person are two reactors -- and the emoji is
 * RAW: it is not folded to a grouping key, so two spellings of one emoji are two reactions and
 * grouping them is yours to do. mine is true when THIS device sealed it.
 * reaction_info answers NULL for either index out of range; free it with urnet_free_string. */
int32_t urnet_message_list_reaction_count(uint64_t self, int32_t index);
char* urnet_message_list_reaction_info(uint64_t self, int32_t index, int32_t reaction_index);

/* the roster's list handle, from urnet_message_group_members: the message list's shape over
 * members. one member as json:
 *   {"leaf_index":u32,"sender_handle":"<32 hex>","identity_pub":"<hex>","role":"owner","mine":bool}
 * leaf_index is the member's leaf in the ratchet tree. sender_handle is the same value a message
 * info carries, so a line joins to its roster row on it. identity_pub is the member's identity
 * public key, lower case hex, and IS THE VALUE THE TWO VERBS TAKE as identity_pub_hex; one
 * identity with several devices appears once per leaf with the same role on each. role is one of
 * "owner", "admin", "member", "observer", and a member the policy does not name is "member". mine
 * is true on this device's own leaf, and on exactly one row.
 *
 * THE KEY LIST ABOVE IS THE JSON'S OWN, IN ITS ORDER, and a go test in this directory holds it so.
 * _info answers NULL for an index out of range; free it with urnet_free_string. */
int32_t urnet_message_member_list_count(uint64_t self);
char* urnet_message_member_list_info(uint64_t self, int32_t index);

#ifdef __cplusplus
}
#endif

#endif
