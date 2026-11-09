// A small in-memory IndexedDB for sidecar.wasm's store tests in Node: the subset cmd/sidecarwasm calls (open with an
// upgrade, transactions over one store, get, put, add, count, delete, a cursor over a key range), with the semantics
// those tests lean on. A transaction's writes land together at its commit or not at all; it commits once no request
// is pending after the task that made its last one, and a request made once it is no longer active throws, as a
// browser's does. Readwrite transactions run one at a time, each starting once the one before it finished, as a
// browser runs those whose scopes overlap, so a read and a write in one of them are atomic. Values are structured
// clones, CryptoKeys included. Keys are strings.
//
// globalThis.fakeIndexedDb is the test's control: faults to arm, an eviction, and raw access to what is stored.
// It is test code only, and stands in for the fake-indexeddb package, which this build does not fetch.

class FakeDomException extends Error {
  constructor(name) {
    super(name);
    this.name = name;
  }
}

class FakeRequest {
  constructor(transaction) {
    this.transaction = transaction;
    this.result = undefined;
    this.error = null;
    this.readyState = "pending";
    this.onsuccess = null;
    this.onerror = null;
  }

  // Fires success or error on a later task, as a browser does.
  settle(result, error) {
    setTimeout(() => {
      this.readyState = "done";
      if (error) {
        this.error = error;
        this.transaction?.requestFailed(this, error);
        return;
      }
      this.result = result;
      this.transaction?.dispatch(() => this.onsuccess?.({target: this}));
      this.transaction?.requestDone();
    }, 0);
  }
}

class FakeKeyRange {
  constructor(lower, upper) {
    this.lower = lower;
    this.upper = upper;
  }

  static bound(lower, upper) {
    return new FakeKeyRange(lower, upper);
  }

  includes(key) {
    return this.lower <= key && key <= this.upper;
  }
}

class FakeObjectStore {
  constructor(transaction, name) {
    this.transaction = transaction;
    this.name = name;
  }

  // What the transaction sees: its own writes over the committed records.
  view(key) {
    const writes = this.transaction.writes.get(this.name);
    if (writes?.has(key)) {
      return writes.get(key);
    }
    return this.transaction.database.stores.get(this.name).get(key);
  }

  request(work) {
    this.transaction.checkActive();
    const request = new FakeRequest(this.transaction);
    this.transaction.pending += 1;
    this.transaction.run(() => {
      let result;
      let error = null;
      try {
        result = work();
      } catch (thrown) {
        error = thrown;
      }
      request.settle(result, error);
    });
    return request;
  }

  get(key) {
    return this.request(() => {
      this.transaction.control.takeFault("read");
      const value = this.view(key);
      return value === undefined ? undefined : structuredClone(value);
    });
  }

  count(key) {
    return this.request(() => (this.view(key) === undefined ? 0 : 1));
  }

  put(value, key) {
    this.transaction.checkWritable();
    const cloned = structuredClone(value);
    return this.request(() => {
      this.transaction.control.takeFault("write");
      this.transaction.write(this.name, key, cloned);
      return key;
    });
  }

  add(value, key) {
    this.transaction.checkWritable();
    const cloned = structuredClone(value);
    return this.request(() => {
      this.transaction.control.takeFault("write");
      if (this.view(key) !== undefined) {
        throw new FakeDomException("ConstraintError");
      }
      this.transaction.write(this.name, key, cloned);
      return key;
    });
  }

  delete(key) {
    this.transaction.checkWritable();
    return this.request(() => {
      this.transaction.write(this.name, key, undefined);
      return undefined;
    });
  }

  openCursor(range) {
    this.transaction.checkActive();
    const keys = new Set([...this.transaction.database.stores.get(this.name).keys()]);
    for (const key of this.transaction.writes.get(this.name)?.keys() ?? []) {
      keys.add(key);
    }
    const request = new FakeRequest(this.transaction);
    let sorted = null;
    let index = 0;
    const step = () => {
      this.transaction.pending += 1;
      sorted ??= [...keys].filter((key) => range.includes(key) && this.view(key) !== undefined).sort();
      const key = sorted[index];
      if (key === undefined) {
        request.settle(null, null);
        return;
      }
      const cursor = {
        key,
        value: structuredClone(this.view(key)),
        continue: () => {
          this.transaction.checkActive();
          index += 1;
          step();
        },
      };
      request.settle(cursor, null);
    };
    this.transaction.run(step);
    return request;
  }
}

class FakeTransaction {
  constructor(control, database, storeNames, mode) {
    this.control = control;
    this.database = database;
    this.storeNames = Array.isArray(storeNames) ? storeNames : [storeNames];
    this.mode = mode;
    this.writes = new Map();
    this.pending = 0;
    this.finished = false;
    this.error = null;
    this.oncomplete = null;
    this.onerror = null;
    this.onabort = null;
    this.active = true;
    // active until the task that made it ends
    queueMicrotask(() => {
      this.active = false;
    });
    // a readwrite transaction waits for the one before it; its requests queue until it starts
    this.started = mode !== "readwrite";
    this.queued = [];
    this.finishedSignal = () => {};
    if (!this.started) {
      const before = database.lastWrite;
      database.lastWrite = new Promise((resolve) => {
        this.finishedSignal = resolve;
      });
      before.then(() => this.start());
    }
    setTimeout(() => this.maybeCommit(), 0);
  }

  // Runs work now once started, else when it starts.
  run(work) {
    if (this.started) {
      work();
    } else {
      this.queued.push(work);
    }
  }

  start() {
    this.started = true;
    for (const work of this.queued.splice(0)) {
      work();
    }
    setTimeout(() => this.maybeCommit(), 0);
  }

  objectStore(name) {
    if (!this.storeNames.includes(name) || !this.database.stores.has(name)) {
      throw new FakeDomException("NotFoundError");
    }
    return new FakeObjectStore(this, name);
  }

  checkActive() {
    if (this.finished || !this.active) {
      throw new FakeDomException("TransactionInactiveError");
    }
  }

  checkWritable() {
    this.checkActive();
    if (this.mode !== "readwrite") {
      throw new FakeDomException("ReadOnlyError");
    }
  }

  write(store, key, value) {
    if (!this.writes.has(store)) {
      this.writes.set(store, new Map());
    }
    this.writes.get(store).set(key, value);
  }

  // Runs a request's handler while the transaction is active, as a browser does.
  dispatch(handler) {
    if (this.finished) {
      return;
    }
    this.active = true;
    try {
      handler();
    } finally {
      queueMicrotask(() => {
        this.active = false;
      });
    }
  }

  requestDone() {
    this.pending -= 1;
    setTimeout(() => this.maybeCommit(), 0);
  }

  requestFailed(request, error) {
    this.pending -= 1;
    this.dispatch(() => request.onerror?.({target: request}));
    this.abort(error);
  }

  maybeCommit() {
    if (this.finished || !this.started || this.pending !== 0 || this.active) {
      return;
    }
    if (this.mode === "readwrite" && this.writes.size !== 0 && this.control.abortNextCommit) {
      // the tab died before the commit: none of the writes land
      this.control.abortNextCommit = false;
      this.abort(new FakeDomException("AbortError"));
      return;
    }
    this.finished = true;
    this.finishedSignal();
    for (const [store, writes] of this.writes) {
      const records = this.database.stores.get(store);
      for (const [key, value] of writes) {
        if (value === undefined) {
          records.delete(key);
        } else {
          records.set(key, value);
        }
      }
    }
    this.oncomplete?.({target: this});
  }

  abort(error) {
    if (this.finished) {
      return;
    }
    this.finished = true;
    this.finishedSignal();
    this.writes.clear();
    this.error = error ?? new FakeDomException("AbortError");
    if (error) {
      this.onerror?.({target: this});
    }
    this.onabort?.({target: this});
  }
}

class FakeDatabase {
  constructor(control, name, version) {
    this.control = control;
    this.name = name;
    this.version = version;
    this.stores = new Map();
    this.closed = false;
    this.objectStoreNames = {contains: (store) => this.stores.has(store)};
    this.lastWrite = Promise.resolve();
  }

  createObjectStore(name) {
    if (this.stores.has(name)) {
      throw new FakeDomException("ConstraintError");
    }
    this.stores.set(name, new Map());
    return {name};
  }

  transaction(storeNames, mode = "readonly", options = undefined) {
    if (this.closed || this.control.evicted.has(this)) {
      throw new FakeDomException("InvalidStateError");
    }
    return new FakeTransaction(this.control, this, storeNames, mode);
  }

  close() {
    this.closed = true;
  }
}

// Installs the fake on target and returns its control.
export function installFakeIndexedDb(target) {
  const control = {
    databases: new Map(),
    evicted: new WeakSet(),
    // the next read or write request fails with this error name, then the fault clears
    readFault: null,
    writeFault: null,
    // the next readwrite transaction with writes aborts at its commit
    abortNextCommit: false,
    // indexedDB.open fails with this error name while set
    openFault: null,

    takeFault(kind) {
      const name = kind === "read" ? this.readFault : this.writeFault;
      if (name) {
        if (kind === "read") {
          this.readFault = null;
        } else {
          this.writeFault = null;
        }
        throw new FakeDomException(name);
      }
    },

    // The browser clears the origin's bucket whole (storage spec §7): every database goes, and a connection open on
    // one can make no more transactions.
    evict() {
      for (const database of this.databases.values()) {
        this.evicted.add(database);
      }
      this.databases.clear();
    },

    // What is stored under key, uncloned, or undefined.
    raw(databaseName, store, key) {
      return this.databases.get(databaseName)?.stores.get(store)?.get(key);
    },

    setRaw(databaseName, store, key, value) {
      const records = this.databases.get(databaseName).stores.get(store);
      if (value === undefined) {
        records.delete(key);
      } else {
        records.set(key, value);
      }
    },

    keys(databaseName, store) {
      return [...(this.databases.get(databaseName)?.stores.get(store)?.keys() ?? [])].sort();
    },
  };

  const factory = {
    open(name, version = 1) {
      const request = new FakeRequest(null);
      request.onupgradeneeded = null;
      request.onblocked = null;
      setTimeout(() => {
        if (control.openFault) {
          request.error = new FakeDomException(control.openFault);
          request.readyState = "done";
          request.onerror?.({target: request});
          return;
        }
        let database = control.databases.get(name);
        if (database && version < database.version) {
          request.error = new FakeDomException("VersionError");
          request.readyState = "done";
          request.onerror?.({target: request});
          return;
        }
        if (!database || database.version < version) {
          const upgraded = database ?? new FakeDatabase(control, name, version);
          upgraded.version = version;
          control.databases.set(name, upgraded);
          database = upgraded;
          request.result = database;
          request.onupgradeneeded?.({target: request, oldVersion: 0, newVersion: version});
        }
        request.result = database;
        request.readyState = "done";
        request.onsuccess?.({target: request});
      }, 0);
      return request;
    },
  };

  target.indexedDB = factory;
  target.IDBKeyRange = FakeKeyRange;
  target.fakeIndexedDb = control;
  return control;
}
