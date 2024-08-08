# WebBridgeBot

**WebBridgeBot** is a Telegram bot designed to bridge the gap between Telegram media content and web browsers. By forwarding video, audio, and photo files to WebBridgeBot, users can generate a web URL that hosts a webpage. This webpage communicates with WebBridgeBot via WebSocket, allowing for instant playback of the media in a web browser. This seamless integration makes it easier than ever to enjoy Telegram media on various devices such as TVs, game consoles, and web kiosks.

## Features

- **Real-time WebSocket Communication:** Instantaneous interaction between the Telegram bot and the web interface, ensuring real-time playback of media files.
- **Stream Media Directly from Telegram:** Download and stream audio and video files from Telegram chats directly to a web interface.
- **User-friendly Web Interface:** Access and play media files through a simple and intuitive web interface, compatible with most modern devices.
- **Easy Navigation from Telegram:** Effortlessly navigate to the web interface using commands within Telegram.
- **Efficient Streaming with Partial Content Delivery:** Supports efficient file streaming with partial content delivery, allowing for responsive playback.

## Prerequisites

Before you start, ensure you have the following prerequisites installed on your system:

- **Docker:** Required for containerized deployment.
- **Go (version 1.21 or newer):** Necessary for building the application as specified in the Dockerfile.
- **Telegram API Credentials:** You need to obtain the `API ID` and `API Hash` from Telegram's [developer portal](https://my.telegram.org/).
- **Telegram Bot Token:** You can create a new bot and obtain a bot token using [BotFather](https://t.me/BotFather) on Telegram.

## Admin Roles and User Authentication

### Admin Roles

When the bot is first initialized, the first user who interacts with it (typically using the `/start` command) is automatically granted admin rights. Admins have the following privileges:

- **Authorize Users:** Admins can authorize new users, allowing them to interact with the bot. This is done using the `/authorize <user_id>` command.
- **Grant Admin Privileges:** Admins can promote other users to admin status by adding the `admin` flag when authorizing a user (`/authorize <user_id> admin`).
- **Receive Notifications:** Admins are notified whenever a new user interacts with the bot. This allows them to decide whether to authorize the user or not.

### User Authentication

WebBridgeBot includes a user authentication mechanism to ensure that only authorized users can interact with the bot and access its web interface:

- **Automatic Authorization for the First User:** The first user to start the bot is automatically authorized and granted admin privileges.
- **Manual Authorization:** All subsequent users must be manually authorized by an admin. This is to prevent unauthorized access to the bot's features.
- **Unauthorized Users:** If a user who is not authorized attempts to interact with the bot, they will receive a message informing them that they need to be authorized by an admin. The bot will also notify the admins about this new user.
- **User Information Storage:** The bot stores user information in a database, which includes whether a user is authorized and whether they have admin privileges.

### Commands Overview

- **/start:** Initializes interaction with the bot. If the user is the first to start the bot, they are granted admin rights.
- **/authorize <user_id> [admin]:** Authorizes a user to interact with the bot. If `admin` is specified, the user is granted admin rights.
- **/deauthorize <user_id>:**  Removes authorization from a user, preventing them from interacting with the bot.

Admins can use these commands to control who can use the bot and manage user roles effectively.

## Setup Instructions

### Cloning the Repository

Begin by cloning the WebBridgeBot repository to your local machine:

```bash
git clone https://github.com/mshafiee/webbridgebot.git
cd webBridgeBot
```

### Building WebBridgeBot

Once you have all dependencies in place, build the WebBridgeBot application:

```bash
make webBridgeBot
```

This command compiles the `webBridgeBot` Go application, creating an executable that can be run on your system.

### Running WebBridgeBot with Docker

To build and run the WebBridgeBot Docker container, use the following commands:

```bash
docker build -t webbridgebot:latest .
docker run -p 8080:8080 \
-e "API_ID=your_api_id" \
-e "API_HASH=your_api_hash" \
-e "BOT_TOKEN=your_bot_token" \
-e "BASE_URL=http://example.com" \
webbridgebot:latest
```

Replace `your_api_id`, `your_api_hash`, and `your_bot_token` with your actual Telegram credentials. Also, adjust `http://example.com` to match the URL where your WebBridgeBot instance will be accessible.

### Running WebBridgeBot with Docker Compose

For a simpler and more streamlined deployment, use Docker Compose to manage the WebBridgeBot service. This approach allows for easier management of environment variables and port mappings.

#### 1. Create a .env File

First, create a `.env` file in the root directory of the project with your Telegram credentials and other necessary configurations:

```plaintext
# .env file content
API_ID=123456
API_HASH=abcdef1234567890abcdef1234567890
BOT_TOKEN=123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11
BASE_URL=http://localhost:8080
PORT=8080
CACHE_DIRECTORY=.cache
```

Make sure to replace the placeholders with your actual data.

#### 2. Running with Docker Compose

To start the WebBridgeBot service using Docker Compose, navigate to the project's root directory and execute:

```bash
docker-compose up -d
```

This command builds the Docker image (if not already built) and starts the container in the background. To view the logs of your service, use:

```bash
docker-compose logs -f
```

To stop and remove the containers, use:

```bash
docker-compose down
```

## Environment Variables

The WebBridgeBot uses several environment variables that must be configured properly:

- **API_ID:** Your Telegram API ID.
- **API_HASH:** Your Telegram API Hash.
- **BOT_TOKEN:** The token for your Telegram bot.
- **BASE_URL:** The base URL where the bot's web interface will be hosted.
- **PORT:** The port on which the web server will run.
- **CACHE_DIRECTORY:** The directory where cached files will be stored.

## Contributing

We welcome contributions to the WebBridgeBot project! To contribute:

1. Fork the repository.
2. Create a new branch with your feature or bugfix.
3. Submit a pull request with a clear description of your changes.

Check the issues tab for ways you can help make WebBridgeBot even better.

## License

WebBridgeBot is licensed under the GNU General Public License v3.0. See the [LICENSE](LICENSE) file for more details.

## Troubleshooting

If you encounter issues during setup or while running the bot, consider the following steps:

- **Ensure all environment variables are correctly set.**
- **Check Docker and Docker Compose versions:** Make sure you are using compatible versions.
- **Review logs:** Use `docker-compose logs -f` to review the output logs for any errors or warnings.
- **Update Dependencies:** Regularly update dependencies to their latest versions to avoid compatibility issues.

For further assistance, please open an issue on the GitHub repository.

## Contact

For any questions or feedback, you can reach out to the maintainers through GitHub or Telegram.
