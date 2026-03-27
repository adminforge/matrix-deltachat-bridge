# Matrix-DeltaChat Bridge

Diese Brücke ermöglicht die bidirektionale Synchronisation von Textnachrichten und Medien (Bilder, Videos, Audio) zwischen einem Matrix-Raum und einer Delta Chat Gruppe.

## Features

- **Bidirektionales Relaying:** Nachrichten werden in Echtzeit zwischen beiden Plattformen gespiegelt.
- **Matrix E2EE:** Volle Unterstützung für Ende-zu-Ende-Verschlüsselung in Matrix-Räumen.
- **Rich Media:** Bilder, Videos und Audio-Dateien werden direkt eingebettet übertragen.
- **Native Reactions:** Emojis werden bidirektional synchronisiert und akkumuliert.
- **Automatisches Setup:** Erstellt bei Bedarf automatisch ein Delta Chat Konto.
- **Einfache Steuerung:** Verwaltung über einfache `/set` Befehle direkt in den Zielräumen.
- **Sicherheit:** Nur autorisierte Admins können den Bot steuern oder in Räume einladen.
- **Privatsphäre:** Keine Protokollierung von Nachrichten-Inhalten in den System-Logs.

## Sicherheit (Hardening)

Der Bot ist für einen sicheren Betrieb vorkonfiguriert:
- **Non-Root:** Läuft unter User `1002:998`.
- **Capabilities:** Alle unnötigen Linux-Capabilities sind entfernt (`cap_drop: ALL`).
- **No New Privileges:** Verhindert Privilegieneskalation innerhalb des Containers.
- **Resource Limits:** Begrenzt auf 0.5 CPU Kerne und 512 MB RAM.

### Wichtig: Dateiberechtigungen
Damit der Bot in den `/data` Ordner schreiben kann, müssen die Berechtigungen auf dem Host-System einmalig angepasst werden:

```bash
chown -R 1002:998 ./data
```

## Schnellstart (Docker Compose)

1.  **Konfiguration:**
    Kopiere die Datei `.env.example` nach `.env` und passe die Werte an:
    - `MATRIX_HOMESERVER`: Die URL deines Matrix-Servers (z.B. `https://matrix.org`).
    - `MATRIX_ADMIN`: Kommagetrennte Liste von Matrix-IDs (z.B. `@alice:server.tld,@bob:server.tld`).
    - `DELTACHAT_ADMIN`: Kommagetrennte Liste von Delta Chat Emails (z.B. `alice@dc.tld,bob@dc.tld`).

2.  **Starten:**
    ```bash
    docker compose up -d --build
    ```

3.  **Verbindung herstellen:**
    Prüfe die Logs, um die Einladungs-Links zu erhalten:
    ```bash
    docker compose logs bridge
    ```
    - **Delta Chat:** Klicke auf den Link in der Zeile `>>> https://i.delta.chat/... <<<`.
    - **Matrix:** Nutze den Link `https://matrix.to/#/@botid:server.tld`, um den Bot zu kontaktieren.

## Einrichtung der Brücke

Sobald der Bot läuft und du ihn als Admin kontaktiert hast:

### 1. Matrix-Seite
- Lade den Bot in den gewünschten Raum ein. (Nur Admins können den Bot erfolgreich einladen).
- Schreibe innerhalb dieses Raums den Befehl: `/set`
- Der Bot bestätigt die Aktivierung der Brücke für diesen Raum.

### 2. Delta Chat Seite
- Lade den Bot in die gewünschte Gruppe ein. (Nur Admins können den Bot erfolgreich einladen).
- Schreibe innerhalb dieser Gruppe den Befehl: `/set`
- Der Bot bestätigt die Aktivierung der Brücke für diesen Chat.

---
Betrieben mit ❤️ und erstellt mit 🤖 von adminForge.
