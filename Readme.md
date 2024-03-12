# WebBridgeBot

WebBridgeBot is a Telegram bot designed to effortlessly stream media content from Telegram directly to a web browser. Users have the capability to forward their video and audio files to the bot, which then generates a web URL. This URL hosts a webpage that communicates with WebBridgeBot via WebSocket. Instantly upon receiving the forwarded message, it begins playback of the media on a web browser. This functionality simplifies the process of forwarding and playing media files from Telegram on various devices such as TVs, game consoles, and web kiosks. The README below provides detailed instructions on how to set up and operate WebBridgeBot.

## Features

- WebSocket communication for real-time interaction.
- Ability to download and stream audio and video files from Telegram chats.
- Web interface to access and play media files.
- Supports commands within Telegram to facilitate easy access to the web interface.
- Automatic handling of file downloads and streaming with support for partial content delivery.

## Prerequisites

- Telegram API credentials (API ID and API Hash)
- A Telegram Bot Token

Before you start, ensure you have the following installed:
- Go (version 1.13 or later)
- Git
- CMake
- OpenSSL
- g++
- Make

## Cloning the Repository

First, clone the WebBridgeBot repository to your local machine:

```bash
git clone https://github.com/mshafiee/webbridgebot.git
cd webBridgeBot
```

## Building TDLib

WebBridgeBot relies on TDLib (Telegram Database Library) for interacting with the Telegram API. Use the included Makefile to clone and build TDLib:

```bash
make all
```

This command clones the TDLib repository, builds it, and installs the necessary files in the `tdlib_install` directory.

## Building WebBridgeBot

To build WebBridgeBot, set the CGO flags for your environment and use the Go build command:

```bash
CGO_CFLAGS="-I$(pwd)/tdlib_install/include -I/opt/homebrew/Cellar/openssl@3/3.2.1/include" \
CGO_LDFLAGS="-L$(pwd)/tdlib_install/lib -L/opt/homebrew/Cellar/openssl@3/3.2.1/lib -lssl -lcrypto" \
go build
```

This command should be run from the root directory of your WebBridgeBot project. Adjust the paths according to your OpenSSL installation and the location of your `tdlib_install` directory.

## Running WebBridgeBot

To run the bot, you need to provide it with several command line flags:

- `apiID`: Your Telegram API ID.
- `apiHash`: Your Telegram API Hash.
- `botToken`: The token for your Telegram bot.
- `baseURL`: The base URL for the webhook.
- `port`: The port on which the bot runs.
- `local`: (Optional) Use the machine's IP address as the base URL. Pass `true` to enable.

```bash
./webBridgeBot -apiID=123456 -apiHash="yourapihash" -botToken="yourbottoken" -baseURL="http://example.com" -port="8080"
```

Replace the values for `apiID`, `apiHash`, `botToken`, and `baseURL` with your specific details.

## Contributing

Contributions to WebBridgeBot are welcome.

## License

This project is licensed under the GNU General Public License v3.0 - see the [LICENSE](LICENSE) file for details.

