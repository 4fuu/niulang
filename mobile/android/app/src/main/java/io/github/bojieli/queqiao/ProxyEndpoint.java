package io.github.bojieli.queqiao;

import android.content.Context;
import android.util.Base64;

import org.json.JSONObject;

import java.security.GeneralSecurityException;
import java.security.SecureRandom;

/**
 * Where the exported SOCKS5 listener binds and what it demands from a client.
 *
 * Loopback is shared with every other app on the device, so the listener is not
 * a private channel: the credentials are the only thing standing between the
 * gateway and any app that happens to guess the port. They are generated per
 * install and stored through the Keystore-backed SecureStore alongside the
 * device key.
 */
final class ProxyEndpoint {
    static final String HOST = "127.0.0.1";
    static final int DEFAULT_PORT = 1080;
    /** Below 1024 Android refuses the bind; the Go core rejects it too. */
    static final int MINIMUM_PORT = 1024;
    static final int MAXIMUM_PORT = 65535;

    private static final String SECRET_NAME = "proxy_endpoint";
    private static final String FIELD_PORT = "port";
    private static final String FIELD_USERNAME = "username";
    private static final String FIELD_PASSWORD = "password";
    private static final int USERNAME_BYTES = 6;
    private static final int PASSWORD_BYTES = 18;

    final int port;
    final String username;
    final String password;

    private ProxyEndpoint(int port, String username, String password) {
        this.port = port;
        this.username = username;
        this.password = password;
    }

    String listenAddress() {
        return HOST + ":" + port;
    }

    /** Reads the stored endpoint, generating credentials on first use. */
    static ProxyEndpoint load(Context context) throws GeneralSecurityException {
        SecureStore store = new SecureStore(context);
        String encoded = store.get(SECRET_NAME);
        if (encoded != null) {
            try {
                JSONObject record = new JSONObject(encoded);
                int port = record.optInt(FIELD_PORT, DEFAULT_PORT);
                String username = record.getString(FIELD_USERNAME);
                String password = record.getString(FIELD_PASSWORD);
                if (validPort(port) && !username.isEmpty() && !password.isEmpty()) {
                    return new ProxyEndpoint(port, username, password);
                }
            } catch (Exception exception) {
                throw new GeneralSecurityException(
                        "The stored proxy endpoint is unreadable; regenerate the credentials", exception);
            }
        }
        return save(store, generate(DEFAULT_PORT));
    }

    /**
     * Issues new credentials, keeping the port. Every configured client stops
     * working until it is updated, which is the point of the action.
     */
    static ProxyEndpoint regenerate(Context context) throws GeneralSecurityException {
        return save(new SecureStore(context), generate(load(context).port));
    }

    static ProxyEndpoint withPort(Context context, int port) throws GeneralSecurityException {
        if (!validPort(port)) {
            throw new GeneralSecurityException(
                    "Choose a port between " + MINIMUM_PORT + " and " + MAXIMUM_PORT);
        }
        ProxyEndpoint current = load(context);
        return save(new SecureStore(context), new ProxyEndpoint(port, current.username, current.password));
    }

    static boolean validPort(int port) {
        return port >= MINIMUM_PORT && port <= MAXIMUM_PORT;
    }

    private static ProxyEndpoint save(SecureStore store, ProxyEndpoint endpoint)
            throws GeneralSecurityException {
        try {
            store.put(SECRET_NAME, new JSONObject()
                    .put(FIELD_PORT, endpoint.port)
                    .put(FIELD_USERNAME, endpoint.username)
                    .put(FIELD_PASSWORD, endpoint.password)
                    .toString());
        } catch (org.json.JSONException exception) {
            throw new GeneralSecurityException("could not encode the proxy endpoint", exception);
        }
        return endpoint;
    }

    private static ProxyEndpoint generate(int port) {
        SecureRandom random = new SecureRandom();
        return new ProxyEndpoint(port, "qq-" + token(random, USERNAME_BYTES), token(random, PASSWORD_BYTES));
    }

    /** URL-safe so the value survives being pasted into JSON, YAML, or a URI. */
    private static String token(SecureRandom random, int bytes) {
        byte[] material = new byte[bytes];
        random.nextBytes(material);
        return Base64.encodeToString(
                material, Base64.NO_WRAP | Base64.NO_PADDING | Base64.URL_SAFE);
    }
}
