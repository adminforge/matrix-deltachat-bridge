# Matrix-DeltaChat Bridge

Diese Brücke ermöglicht die bidirektionale Synchronisation von Textnachrichten und Medien (Bilder, Videos, Audio) zwischen einem Matrix-Raum und einer Delta Chat Gruppe.

## Features

- **Bidirektionales Relaying:** Nachrichten werden in Echtzeit zwischen beiden Plattformen gespiegelt.
- **Matrix E2EE:** Volle Unterstützung für Ende-zu-Ende-Verschlüsselung in Matrix-Räumen.
- **Rich Media:** Bilder, Videos und Audio-Dateien werden direkt eingebettet übertragen.
- **Native Replies:** Bidirectional synchronization of message replies.
- **Native Reactions:** Emojis werden bidirektional synchronisiert und akkumuliert.
- **Automatisches Setup:** Erstellt bei Bedarf automatisch ein Delta Chat Konto.
- **Einfache Steuerung:** Verwaltung über einfache `/set` Befehle direkt in den Zielräumen.
- **Sicherheit:** Nur autorisierte Admins können den Bot steuern oder in Räume einladen.

## Schnellstart (Docker Compose)

Führe diese Befehle in einem neuen Verzeichnis auf deinem Server aus:

1.  **Dateien herunterladen:**
    ```bash
    wget https://git.adminforge.de/adminforge/matrix-deltachat-bridge/raw/branch/main/docker-compose.yml
    wget https://git.adminforge.de/adminforge/matrix-deltachat-bridge/raw/branch/main/.env.example
    ```

2.  **Konfiguration:**
    ```bash
    mv .env.example .env
    ```
    Passe die Werte in der `.env` an:
    - `MATRIX_HOMESERVER`: Die URL deines Matrix-Servers (z.B. `https://matrix.org`).
    - `MATRIX_ADMIN`: Deine Matrix-ID (z.B. `@alice:server.tld`).
    - `MATRIX_PICKLE_KEY`: Generiere einen Schlüssel mit `openssl rand -hex 32`.
    - `DELTACHAT_ADMIN`: Deine Delta Chat Email (z.B. `alice@dc.tld`).
    - `DELTACHAT_RELAY_SERVER`: Der Mail-Server (z.B. `chat.adminforge.de`).
    - `BRIDGE_USER` & `BRIDGE_GROUP`: Die User/Group ID unter der der Bot laufen soll (Standard: 1002:998).

    **Datenverzeichnis vorbereiten:**
    ```bash
    mkdir -p ./data
    export $(grep -v '^#' .env | xargs) && chown -R $BRIDGE_USER:$BRIDGE_GROUP ./data
    ```

3.  **Starten:**
    ```bash
    docker compose up -d --build
    ```

4.  **Verbindung herstellen:**
    Prüfe die Logs, um die Einladungs-Links zu erhalten:
    ```bash
    docker compose logs bridge
    ```
    - **Delta Chat:** Klicke auf den Link in der Zeile `>>> https://i.delta.chat/... <<<`.
    - **Matrix:** Nutze den Link `https://matrix.to/#/@botid:server.tld`, um den Bot zu kontaktieren.

## Einrichtung der Brücke

Sobald der Bot läuft und du ihn als Admin kontaktiert hast:

### 1. Matrix-Seite
- Lade den Bot in den gewünschten Raum ein.
- Schreibe innerhalb dieses Raums den Befehl: `/set`

### 2. Delta Chat Seite
- Lade den Bot in die gewünschte Gruppe ein.
- Schreibe innerhalb dieser Gruppe den Befehl: `/set`

---
Betrieben mit ❤️ und erstellt mit 🤖 von adminForge.
