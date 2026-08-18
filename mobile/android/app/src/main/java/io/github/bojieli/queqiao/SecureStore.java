package io.github.bojieli.queqiao;

import android.content.Context;
import android.content.SharedPreferences;
import android.security.keystore.KeyGenParameterSpec;
import android.security.keystore.KeyProperties;
import android.util.Base64;

import java.io.IOException;
import java.nio.ByteBuffer;
import java.nio.charset.StandardCharsets;
import java.security.GeneralSecurityException;
import java.security.KeyStore;

import javax.crypto.Cipher;
import javax.crypto.KeyGenerator;
import javax.crypto.SecretKey;
import javax.crypto.spec.GCMParameterSpec;

final class SecureStore {
    static final String PROFILE = "client_profile";
    static final String ENROLLMENT_DRAFT = "enrollment_draft";

    private static final String PREFERENCES = "queqiao_secure_store";
    private static final String KEY_ALIAS = "queqiao.mobile.profile.aes.v1";
    private static final int FORMAT_VERSION = 2;
    private static final int GCM_TAG_BITS = 128;

    private final SharedPreferences preferences;

    SecureStore(Context context) {
        preferences = context.getApplicationContext()
                .getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE);
    }

    synchronized void put(String name, String plaintext) throws GeneralSecurityException {
        if (plaintext == null || plaintext.isEmpty()) {
            throw new GeneralSecurityException("refusing to store an empty secret");
        }
        Cipher cipher = Cipher.getInstance("AES/GCM/NoPadding");
        cipher.init(Cipher.ENCRYPT_MODE, getOrCreateKey());
        cipher.updateAAD(name.getBytes(StandardCharsets.UTF_8));
        byte[] encrypted = cipher.doFinal(plaintext.getBytes(StandardCharsets.UTF_8));
        byte[] iv = cipher.getIV();
        if (iv == null || iv.length == 0 || iv.length > 255) {
            throw new GeneralSecurityException("Android Keystore returned an invalid GCM nonce");
        }
        ByteBuffer envelope = ByteBuffer.allocate(2 + iv.length + encrypted.length);
        envelope.put((byte) FORMAT_VERSION);
        envelope.put((byte) iv.length);
        envelope.put(iv);
        envelope.put(encrypted);
        String encoded = Base64.encodeToString(envelope.array(), Base64.NO_WRAP);
        if (!preferences.edit().putString(name, encoded).commit()) {
            throw new GeneralSecurityException("failed to commit encrypted secret");
        }
    }

    synchronized String get(String name) throws GeneralSecurityException {
        String encoded = preferences.getString(name, null);
        if (encoded == null) {
            return null;
        }
        final byte[] envelope;
        try {
            envelope = Base64.decode(encoded, Base64.NO_WRAP);
        } catch (IllegalArgumentException exception) {
            throw new GeneralSecurityException("encrypted secret is not valid base64", exception);
        }
        if (envelope.length < 3) {
            throw new GeneralSecurityException("encrypted secret is truncated");
        }
        ByteBuffer buffer = ByteBuffer.wrap(envelope);
        int version = Byte.toUnsignedInt(buffer.get());
        int ivLength = Byte.toUnsignedInt(buffer.get());
        if (version != FORMAT_VERSION || ivLength == 0 || buffer.remaining() <= ivLength) {
            throw new GeneralSecurityException("encrypted secret envelope is invalid");
        }
        byte[] iv = new byte[ivLength];
        buffer.get(iv);
        byte[] encrypted = new byte[buffer.remaining()];
        buffer.get(encrypted);
        Cipher cipher = Cipher.getInstance("AES/GCM/NoPadding");
        cipher.init(Cipher.DECRYPT_MODE, getOrCreateKey(), new GCMParameterSpec(GCM_TAG_BITS, iv));
        cipher.updateAAD(name.getBytes(StandardCharsets.UTF_8));
        return new String(cipher.doFinal(encrypted), StandardCharsets.UTF_8);
    }

    synchronized boolean contains(String name) {
        return preferences.contains(name);
    }

    synchronized void delete(String name) throws GeneralSecurityException {
        if (!preferences.edit().remove(name).commit()) {
            throw new GeneralSecurityException("failed to delete encrypted secret");
        }
    }

    private SecretKey getOrCreateKey() throws GeneralSecurityException {
        KeyStore keyStore = KeyStore.getInstance("AndroidKeyStore");
        try {
            keyStore.load(null);
        } catch (IOException exception) {
            throw new GeneralSecurityException("could not load Android Keystore", exception);
        }
        KeyStore.Entry entry = keyStore.getEntry(KEY_ALIAS, null);
        if (entry instanceof KeyStore.SecretKeyEntry) {
            return ((KeyStore.SecretKeyEntry) entry).getSecretKey();
        }
        if (entry != null) {
            throw new GeneralSecurityException("unexpected Android Keystore entry type");
        }
        KeyGenerator generator = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore");
        generator.init(new KeyGenParameterSpec.Builder(
                KEY_ALIAS,
                KeyProperties.PURPOSE_ENCRYPT | KeyProperties.PURPOSE_DECRYPT)
                .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                .setKeySize(256)
                .setRandomizedEncryptionRequired(true)
                .build());
        return generator.generateKey();
    }
}
