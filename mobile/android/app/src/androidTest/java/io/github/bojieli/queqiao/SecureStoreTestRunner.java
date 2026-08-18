package io.github.bojieli.queqiao;

import android.app.Activity;
import android.app.Instrumentation;
import android.os.Bundle;
import android.util.Log;

/** Dependency-free on-device checks for the Android Keystore envelope. */
public final class SecureStoreTestRunner extends Instrumentation {
    private static final int STATUS_START = 1;
    private static final int STATUS_OK = 0;
    private static final int STATUS_FAILURE = -2;

    @Override
    public void onCreate(Bundle arguments) {
        super.onCreate(arguments);
        start();
    }

    @Override
    public void onStart() {
        NamedCheck[] checks = {
                new NamedCheck("encryptedRoundTripAndDeletion", this::testEncryptedRoundTripAndDeletion),
                new NamedCheck("emptySecretIsRejected", this::testEmptySecretIsRejected),
                new NamedCheck("ciphertextCannotMoveBetweenAccounts", this::testCiphertextCannotBeMovedBetweenAccounts),
                new NamedCheck("profileCatalogRoundTripAndNormalization", this::testProfileCatalogRoundTripAndNormalization),
                new NamedCheck("localNetworkRoutePolicy", this::testLocalNetworkRoutePolicy)
        };
        for (int index = 0; index < checks.length; index++) {
            NamedCheck check = checks[index];
            Bundle status = status(check.name, index + 1, checks.length);
            sendStatus(STATUS_START, status);
            try {
                check.check.run();
                status.putString("stream", ".");
                sendStatus(STATUS_OK, status);
            } catch (Throwable failure) {
                status.putString("stack", Log.getStackTraceString(failure));
                status.putString("stream", "\nFAILED: " + check.name + ": " + failure + "\n");
                sendStatus(STATUS_FAILURE, status);
                finish(Activity.RESULT_CANCELED, status);
                return;
            }
        }
        Bundle result = new Bundle();
        result.putString("stream", "\nOK (5 mobile storage and routing checks)\n");
        finish(Activity.RESULT_OK, result);
    }

    private void testEncryptedRoundTripAndDeletion() throws Exception {
        SecureStore store = new SecureStore(getTargetContext());
        String name = "instrumentation_test_secret";
        store.delete(name);
        store.put(name, "private-profile-value");
        require("private-profile-value".equals(store.get(name)), "encrypted round-trip changed the value");
        require(store.contains(name), "stored value was not reported present");
        store.delete(name);
        require(store.get(name) == null, "deleted value remains readable");
    }

    private void testEmptySecretIsRejected() throws Exception {
        SecureStore store = new SecureStore(getTargetContext());
        try {
            store.put("instrumentation_test_empty", "");
            throw new AssertionError("empty secret was accepted");
        } catch (java.security.GeneralSecurityException expected) {
            // Expected.
        }
    }

    private void testCiphertextCannotBeMovedBetweenAccounts() throws Exception {
        SecureStore store = new SecureStore(getTargetContext());
        String first = "instrumentation_test_first";
        String second = "instrumentation_test_second";
        store.delete(first);
        store.delete(second);
        store.put(first, "domain-separated-value");
        android.content.SharedPreferences preferences = getTargetContext().getSharedPreferences(
                "queqiao_secure_store", android.content.Context.MODE_PRIVATE);
        String envelope = preferences.getString(first, null);
        require(envelope != null, "encrypted envelope was not persisted");
        require(preferences.edit().putString(second, envelope).commit(), "could not move test envelope");
        try {
            store.get(second);
            throw new AssertionError("ciphertext was accepted under a different account name");
        } catch (java.security.GeneralSecurityException expected) {
            // Expected: the account name is authenticated as AES-GCM associated data.
        } finally {
            store.delete(first);
            store.delete(second);
        }
    }

    private void testProfileCatalogRoundTripAndNormalization() throws Exception {
        ProfileRepository.ProfileSummary summary = new ProfileRepository.ProfileSummary(
                1,
                "Example",
                "gateway.example:443",
                "provider",
                "gateway",
                "account",
                "device",
                "Phone",
                "2030-01-01T00:00:00Z");
        ProfileRepository.ProfileRecord profile = new ProfileRepository.ProfileRecord(
                "first",
                "secret.first",
                "Example",
                summary,
                TrafficPolicy.EXCLUDE_LOCAL_NETWORKS,
                "2026-08-18T00:00:00Z");
        ProfileRepository.Catalog catalog = new ProfileRepository.Catalog();
        catalog.selectedProfileId = "missing";
        catalog.profiles.add(profile);
        catalog.profiles.add(profile);

        require(catalog.normalize(), "invalid catalog was not normalized");
        require(catalog.profiles.size() == 1, "duplicate profile was retained");
        require("first".equals(catalog.selectedProfileId), "missing selection was not repaired");

        ProfileRepository.Catalog decoded = ProfileRepository.Catalog.fromJson(catalog.toJson());
        require(decoded.profiles.size() == 1, "catalog round-trip lost its profile");
        require(decoded.profiles.get(0).trafficPolicy == TrafficPolicy.EXCLUDE_LOCAL_NETWORKS,
                "catalog round-trip changed the traffic policy");
    }

    private void testLocalNetworkRoutePolicy() throws Exception {
        java.util.List<RoutePolicy.RouteSpec> routes = RoutePolicy.routesExcludingLocalNetworks();
        require(isRouted(routes, "8.8.8.8"), "public IPv4 address is not routed");
        require(isRouted(routes, "2001:4860:4860::8888"), "public IPv6 address is not routed");
        require(!isRouted(routes, "10.0.0.1"), "private IPv4 address is routed");
        require(!isRouted(routes, "192.168.1.1"), "local IPv4 address is routed");
        require(!isRouted(routes, "127.0.0.1"), "loopback IPv4 address is routed");
        require(!isRouted(routes, "fd00::1"), "unique-local IPv6 address is routed");
        require(!isRouted(routes, "fe80::1"), "link-local IPv6 address is routed");
        require(!isRouted(routes, "::1"), "loopback IPv6 address is routed");
    }

    private boolean isRouted(java.util.List<RoutePolicy.RouteSpec> routes, String address) throws Exception {
        for (RoutePolicy.RouteSpec route : routes) {
            if (route.contains(address)) {
                return true;
            }
        }
        return false;
    }

    private static void require(boolean condition, String message) {
        if (!condition) {
            throw new AssertionError(message);
        }
    }

    private static Bundle status(String name, int current, int total) {
        Bundle status = new Bundle();
        status.putString("class", SecureStoreTestRunner.class.getName());
        status.putString("test", name);
        status.putInt("current", current);
        status.putInt("numtests", total);
        return status;
    }

    @FunctionalInterface
    private interface Check {
        void run() throws Exception;
    }

    private static final class NamedCheck {
        private final String name;
        private final Check check;

        private NamedCheck(String name, Check check) {
            this.name = name;
            this.check = check;
        }
    }
}
