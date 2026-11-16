// The Swift adapter harness (SDK-M8-15; sidecar §22.5's §18 row, D-SDK-15): an app's SidecarKeyStore over the platform's
// keychain never throws, each platform outcome answering one of §5.9's statuses, and a Sidecar opened on each answers
// one terminal state with no crash: Locked and Failed leave it locked and create nothing; Absent with nothing recorded
// lets it create its root key. It runs as an XCTest bundle linked to URnetworkSdk.xcframework (gomobile bind -target
// ios), which needs Xcode and a signed test host; sdk/build/sidecar_adapter_test.go holds every binding name it uses
// to the generated Objective-C, so it cannot rot unseen.

import URnetworkSdk
import XCTest

// What the platform's keychain answers for one item, as SecItemCopyMatching and SecItemAdd do.
enum PlatformKeychainOutcome {
    case found(String)
    case notFound
    case interactionNotAllowed
    case failure
}

// A platform keychain the test scripts: each call answers the outcome set for it, or throws.
final class ScriptedKeychain {
    var outcome: PlatformKeychainOutcome = .notFound
    var throwsOnCall = false
    var stored: [String: String] = [:]

    struct Failure: Error {}

    func copy(_ name: String) throws -> PlatformKeychainOutcome {
        if throwsOnCall {
            throw Failure()
        }
        if case .found = outcome, let value = stored[name] {
            return .found(value)
        }
        return outcome
    }

    func add(_ name: String, _ value: String) throws -> PlatformKeychainOutcome {
        if throwsOnCall {
            throw Failure()
        }
        stored[name] = value
        return outcome
    }
}

// The app's adapter: the platform's outcomes as §5.9's statuses, a throw as Failed, so nothing crosses the binding.
final class KeychainSidecarKeyStore: NSObject, SdkSidecarKeyStoreProtocol {
    let keychain: ScriptedKeychain

    init(keychain: ScriptedKeychain) {
        self.keychain = keychain
    }

    func putSecret(_ name: String?, valueBase64: String?) -> Int32 {
        guard let name = name, let value = valueBase64 else {
            return SdkSidecarKeyStoreFailed
        }
        do {
            switch try keychain.add(name, value) {
            case .interactionNotAllowed:
                return SdkSidecarKeyStoreLocked
            case .failure:
                return SdkSidecarKeyStoreFailed
            default:
                return SdkSidecarKeyStoreOk
            }
        } catch {
            return SdkSidecarKeyStoreFailed
        }
    }

    func getSecret(_ name: String?) -> SdkSidecarSecret? {
        let secret = SdkSidecarSecret()
        guard let name = name else {
            secret.status = SdkSidecarSecretFailed
            return secret
        }
        do {
            switch try keychain.copy(name) {
            case .found(let value):
                secret.status = SdkSidecarSecretPresent
                secret.valueBase64 = value
            case .notFound:
                secret.status = SdkSidecarSecretAbsent
            case .interactionNotAllowed:
                secret.status = SdkSidecarSecretLocked
            case .failure:
                secret.status = SdkSidecarSecretFailed
            }
        } catch {
            secret.status = SdkSidecarSecretFailed
        }
        return secret
    }

    func deleteSecret(_ name: String?) -> Int32 {
        guard let name = name else {
            return SdkSidecarKeyStoreFailed
        }
        keychain.stored.removeValue(forKey: name)
        return SdkSidecarKeyStoreOk
    }
}

final class SidecarAdapterTests: XCTestCase {
    // A space that discovers nothing: its urls under a reserved name, no storage.
    private func space() -> SdkNetworkSpace? {
        let manager = SdkNewNetworkSpaceManagerNoStorage()
        let values = SdkNetworkSpaceValues()
        values.apiUrl = "https://api.sdktest.example"
        values.platformUrl = "wss://connect.sdktest.example"
        return manager?.updateNetworkSpaceValues(SdkNewNetworkSpaceKey("sdktest.example", "test"), values: values)
    }

    // A Sidecar on store, in a fresh state directory, closed after body.
    private func withSidecar(_ store: KeychainSidecarKeyStore, _ body: (SdkSidecar) -> Void) throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let settings = SdkDefaultSidecarSettings()
        settings?.stateDir = directory.path
        settings?.hostClass = SdkSidecarHostClassIos
        let sidecar = try XCTUnwrap(SdkSidecar(local: space(), keyStore: store, presenceSigner: nil, settings: settings))
        body(sidecar)
        sidecar.close()
    }

    // Each platform outcome, and a throw, answers one terminal state; Locked and Failed leave the Sidecar locked.
    func testEachStatusAnswersOneTerminalState() throws {
        let cases: [(PlatformKeychainOutcome, Bool, String)] = [
            (.interactionNotAllowed, false, "locked"),
            (.failure, false, "locked"),
            (.notFound, true, "locked"),
            (.notFound, false, "unenrolled"),
        ]
        for (outcome, throwsOnCall, state) in cases {
            let keychain = ScriptedKeychain()
            keychain.outcome = outcome
            keychain.throwsOnCall = throwsOnCall
            try withSidecar(KeychainSidecarKeyStore(keychain: keychain)) { sidecar in
                let deadline = Date().addingTimeInterval(30)
                while sidecar.getState() != state && Date() < deadline {
                    RunLoop.current.run(until: Date().addingTimeInterval(0.05))
                }
                XCTAssertEqual(sidecar.getState(), state, "\(outcome) throws \(throwsOnCall)")
            }
            if state == "locked" {
                XCTAssertTrue(keychain.stored.isEmpty, "a locked store had something created in it")
            }
        }
    }
}
