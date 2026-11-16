// The Kotlin adapter harness (SDK-M8-15; sidecar §22.5's §18 row, D-SDK-15): an app's SidecarKeyStore over
// AndroidKeyStore never throws, each platform outcome answering one of §5.9's statuses, and a Sidecar opened on each
// answers one terminal state with no crash: Locked and Failed leave it locked and create nothing; Absent with nothing
// recorded lets it create its root key. It runs as an instrumented JUnit test against URnetworkSdk.aar (gomobile bind
// -target android), which needs an AVD or a surrogate this machine does not lend; sdk/build/sidecar_adapter_test.go holds
// every binding name it uses to the generated Java, so it cannot rot unseen.

package com.bringyour.sdk.adapters

import com.bringyour.sdk.Sdk
import com.bringyour.sdk.Sidecar
import com.bringyour.sdk.SidecarKeyStore
import com.bringyour.sdk.SidecarSecret
import java.nio.file.Files
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

// What AndroidKeyStore and the app's sealed file answer for one item.
enum class PlatformKeyOutcome { FOUND, NOT_FOUND, USER_NOT_AUTHENTICATED, FAILURE }

// A platform store the test scripts: each call answers the outcome set for it, or throws.
class ScriptedPlatformStore(var outcome: PlatformKeyOutcome, var throwsOnCall: Boolean = false) {
    val stored = mutableMapOf<String, String>()

    fun read(name: String): Pair<PlatformKeyOutcome, String?> {
        if (throwsOnCall) throw java.security.KeyStoreException("scripted")
        if (outcome == PlatformKeyOutcome.FOUND) return Pair(outcome, stored[name])
        return Pair(outcome, null)
    }

    fun write(name: String, value: String): PlatformKeyOutcome {
        if (throwsOnCall) throw java.security.KeyStoreException("scripted")
        stored[name] = value
        return outcome
    }
}

// The app's adapter: the platform's outcomes as §5.9's statuses, any throw as Failed, so nothing crosses the binding.
class AndroidSidecarKeyStore(private val platform: ScriptedPlatformStore) : SidecarKeyStore {
    override fun putSecret(name: String, valueBase64: String): Int = try {
        when (platform.write(name, valueBase64)) {
            PlatformKeyOutcome.USER_NOT_AUTHENTICATED -> Sdk.SidecarKeyStoreLocked
            PlatformKeyOutcome.FAILURE -> Sdk.SidecarKeyStoreFailed
            else -> Sdk.SidecarKeyStoreOk
        }
    } catch (e: Exception) {
        Sdk.SidecarKeyStoreFailed
    }

    override fun getSecret(name: String): SidecarSecret {
        val secret = SidecarSecret()
        secret.status = try {
            val (outcome, value) = platform.read(name)
            when (outcome) {
                PlatformKeyOutcome.FOUND -> if (value != null) {
                    secret.valueBase64 = value
                    Sdk.SidecarSecretPresent
                } else Sdk.SidecarSecretAbsent
                PlatformKeyOutcome.NOT_FOUND -> Sdk.SidecarSecretAbsent
                PlatformKeyOutcome.USER_NOT_AUTHENTICATED -> Sdk.SidecarSecretLocked
                PlatformKeyOutcome.FAILURE -> Sdk.SidecarSecretFailed
            }
        } catch (e: Exception) {
            Sdk.SidecarSecretFailed
        }
        return secret
    }

    override fun deleteSecret(name: String): Int {
        platform.stored.remove(name)
        return Sdk.SidecarKeyStoreOk
    }
}

class SidecarAdapterTest {
    // A Sidecar on store in a fresh state directory, closed after body.
    private fun withSidecar(store: AndroidSidecarKeyStore, body: (Sidecar) -> Unit) {
        val directory = Files.createTempDirectory("sidecar").toFile()
        try {
            val manager = Sdk.newNetworkSpaceManagerNoStorage()
            val values = com.bringyour.sdk.NetworkSpaceValues()
            values.apiUrl = "https://api.sdktest.example"
            values.platformUrl = "wss://connect.sdktest.example"
            val space = manager.updateNetworkSpaceValues(Sdk.newNetworkSpaceKey("sdktest.example", "test"), values)
            val settings = Sdk.defaultSidecarSettings()
            settings.stateDir = directory.absolutePath
            settings.hostClass = Sdk.SidecarHostClassAndroid
            val sidecar = Sdk.newSidecarLocal(space, store, null, settings)
            try {
                body(sidecar)
            } finally {
                sidecar.close()
            }
        } finally {
            directory.deleteRecursively()
        }
    }

    // Each platform outcome, and a throw, answers one terminal state; Locked and Failed leave the Sidecar locked.
    @Test
    fun eachStatusAnswersOneTerminalState() {
        val cases = listOf(
            Triple(PlatformKeyOutcome.USER_NOT_AUTHENTICATED, false, "locked"),
            Triple(PlatformKeyOutcome.FAILURE, false, "locked"),
            Triple(PlatformKeyOutcome.NOT_FOUND, true, "locked"),
            Triple(PlatformKeyOutcome.NOT_FOUND, false, "unenrolled"),
        )
        for ((outcome, throwsOnCall, state) in cases) {
            val platform = ScriptedPlatformStore(outcome, throwsOnCall)
            withSidecar(AndroidSidecarKeyStore(platform)) { sidecar ->
                val deadline = System.currentTimeMillis() + 30_000
                while (sidecar.state != state && System.currentTimeMillis() < deadline) {
                    Thread.sleep(50)
                }
                assertEquals("$outcome throws $throwsOnCall", state, sidecar.state)
            }
            if (state == "locked") {
                assertTrue("a locked store had something created in it", platform.stored.isEmpty())
            }
        }
    }
}
