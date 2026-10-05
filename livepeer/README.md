# livepeer — the second party

`liveprobe` drives BOTH sides of a conversation in one process. `livepeer` drives ONE side, so the
other side can be a different process: the Windows app.

## Why it exists

A solo device cannot open a group. `Group.Open` comes after `AddMember`, the alpha accepts exactly
one `AddMember`, and one credential means one key package — so a client holding a single credential
founds a group at epoch 0 that never opens, and every send is refused. The missing half is a second
party under a **second account**, and this is it.

## The seam: two files, each deleted by whoever consumes it

```
  the app (user1)                              livepeer (user2)
  ───────────────                              ────────────────
  device_key_package() -> -keypackage     →    reads it, deletes it
                                               CreateGroup, AddMember(kp), Open
  reads it, deletes it                    ←    invite.Encode() -> -invite
  device_join(invite)                          Send / SendReply / React, then fetch in a loop
```

A key package is **single use** — the private halves are taken destructively at the join — so a
stale key-package file read by a later run builds an invite the app *cannot* join, and the failure
lands at the join rather than at the read. Deleting on consumption is what makes "the file is
there" mean "the file is fresh".

## Running it

```
go build -o livepeer.exe .

./livepeer.exe \
  -jwt        %LOCALAPPDATA%\URmessage\dev\user2.jwt \
  -dir        %LOCALAPPDATA%\URmessage\dev\peer \
  -keypackage %LOCALAPPDATA%\URmessage\dev\app.keypackage \
  -invite     %LOCALAPPDATA%\URmessage\dev\app.invite \
  -serve 4m
```

Then launch the app with `--live`. Order does not matter: each side polls for the other's file.
The app writes its key package when it holds no group, and restores from disk on every later run —
so the handshake happens **once** and a second run of this helper against an app that already
joined will simply wait, because no key package is published.

## Rules that are not style

* **NEVER run this under the app's own account.** Two clients at one `client_id`, or two state
  directories descended from one, is two devices at one MLS leaf: one `sender_handle`, one stream
  counter, therefore a reused `(epoch, sender_handle, stream_index)` — a reused nonce under a reused
  record key, which the spec calls a total break of both AEADs. The app is user1; this is user2;
  each gets its own `-dir`.
* **The credential is a secret.** It is read from a path and never printed; the only thing said
  about it is its length.
* **The invite is key material in full.** Whoever reads it is in the group. It is written 0600, to
  a path outside any repository, and deleted as soon as it has been used. Neither file may ever
  enter a working tree.
* **`-dir` must not be inside a repo either** — it holds the durable MLS state.

## What it asserts, and what it does not

It checks the Hello answered `REASON_OK`, that a freshly founded group is at epoch 0, that the
commit adding the app leaves it at epoch 1, and that `Open` actually reports the group open. It
does **not** assert anything about the far side: a member joining is invisible from here, because
the Welcome is opened by the joiner and the server is told nothing. The only signal that the app
joined is that it deleted the invite, and that is reported as a signal rather than as proof.
