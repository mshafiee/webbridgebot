# WebBridgeBot

WebBridgeBot is a Telegram bot that acts as a bridge between Telegram and your web browser. It allows you to forward any video, audio, or photo file to the bot and instantly receive a private link. This link opens a web-based media player that streams the content directly from the bot, enabling seamless playback on devices like TVs, game consoles, or any modern web browser.

---

### ✨ Features

- **Direct Media Streaming:** Stream videos, music, and photos from Telegram directly to a web interface without needing to download them first.
- **Instant Playback:** Utilizes WebSockets for real-time communication between the bot and the web player, allowing for instant media loading and control.
- **Responsive Web Player:** A clean, modern web interface that works on desktops, tablets, and mobile devices. Includes a visualizer for audio files.
- **Secure User Management:** Features a robust authorization system. The first user becomes an admin, who can then authorize or grant admin rights to other users.
- **Efficient Caching:** Caches downloaded file chunks on disk to reduce redundant downloads from Telegram and provide faster access to frequently played media.
- **Partial Content Streaming:** Supports HTTP range requests, allowing browsers to seek through media and stream content efficiently, which is crucial for large files.

### ⚙️ How It Works

1.  **Send Media:** You forward or upload a media file (video, audio, photo) to the bot in a private chat.
2.  **Generate Link:** The bot processes the file, generates a unique, secure URL, and sends it back to you with a control panel.
3.  **Open Player:** You open the URL in any browser. The web page establishes a WebSocket connection back to the bot.
4.  **Play Media:** The bot sends media information (like filename and type) to the player via WebSocket. The player then starts streaming the file content directly from the bot's server.

### 📋 Prerequisites

- **Docker & Docker Compose:** Required for the recommended containerized deployment.
- **Go (1.21+):** Needed only if you plan to build the application from source manually.
- **Telegram API Credentials:**
    - `API ID` and `API Hash`: Obtain these from [my.telegram.org](https://my.telegram.org/).
    - `Bot Token`: Create a bot and get the token from [@BotFather](https://t.me/BotFather) on Telegram.

### 🔑 User & Admin Management

The bot includes a secure authentication system to control access.

-   **First Admin:** The very first user to interact with the bot (by sending `/start`) is automatically granted admin privileges.
-   **Admin Powers:** Admins receive notifications for new users and can manage access with the following commands.
-   **Authorization:** All subsequent users must be manually authorized by an admin before they can use the bot. Unauthorized users will be prompted to request access.

#### Admin Commands

-   `/authorize <user_id>`: Authorizes a user to use the bot.
-   `/authorize <user_id> admin`: Authorizes a user and grants them admin privileges.
-   `/deauthorize <user_id>`: Revokes a user's access to the bot.
-   `/listusers`: Displays a paginated list of all users and their status.
-   `/userinfo <user_id>`: Shows detailed information for a specific user.

### 🚀 Setup & Deployment (Recommended)

Using Docker Compose is the easiest way to run WebBridgeBot.

**1. Clone the Repository**

```bash
git clone https://github.com/mshafiee/webbridgebot.git
cd webbridgebot
```

**2. Create a `.env` file**

Create a file named `.env` in the project's root directory and paste the following content. Replace the placeholder values with your actual credentials.

```plaintext
# .env - Telegram API Configuration
API_ID=1234567
API_HASH=a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4
BOT_TOKEN=1234567890:ABC-DEF1234ghIkl-zyx57W2v1u123ew11

# Web Server and URL Configuration
# Replace localhost with your server's IP or domain name if it's publicly accessible
BASE_URL=http://localhost:8080
PORT=8080

# (Optional) Cache Configuration
HASH_LENGTH=8
MAX_CACHE_SIZE=10737418240 # 10 GB in bytes
CACHE_DIRECTORY=.cache
```

**3. Run with Docker Compose**

Start the bot in the background:

```bash
docker-compose up -d
```

-   **View logs:** `docker-compose logs -f`
-   **Stop the bot:** `docker-compose down`

### 🔧 Environment Variables

These variables can be set in the `.env` file or directly in your environment.

| Variable          | Description                                                    | Default           | Required |
| ----------------- | -------------------------------------------------------------- | ----------------- | -------- |
| `API_ID`          | Your Telegram API ID.                                          | -                 | **Yes**  |
| `API_HASH`        | Your Telegram API Hash.                                        | -                 | **Yes**  |
| `BOT_TOKEN`       | The token for your Telegram bot.                               | -                 | **Yes**  |
| `BASE_URL`        | The public URL where the bot's web player will be hosted.      | `http://localhost:8080` | **Yes**  |
| `PORT`            | The port on which the web server will run.                     | `8080`            | No       |
| `HASH_LENGTH`     | The length of the short hash used in media URLs.               | `8`               | No       |
| `MAX_CACHE_SIZE`  | Maximum size for the disk cache in bytes.                      | `10737418240` (10GB) | No       |
| `CACHE_DIRECTORY` | The directory to store cached media chunks and the database.   | `.cache`          | No       |
| `DEBUG_MODE`      | Set to `true` to enable verbose logging.                       | `false`           | No       |

### 🤝 Contributing

We welcome contributions! Please feel free to fork the repository, create a feature branch, and submit a pull request. Check the issues tab for ideas on how to help.

### 📄 License

WebBridgeBot is licensed under the **GNU General Public License v3.0**. See the `LICENSE` file for more details.

### 🛠️ Troubleshooting

-   **Check Environment Variables:** Ensure all required variables (`API_ID`, `API_HASH`, `BOT_TOKEN`, `BASE_URL`) are correctly set in your `.env` file.
-   **Review Logs:** Use `docker-compose logs -f` to check for any errors during startup or operation.
-   **Permissions:** Make sure the `.cache` directory has the correct write permissions for the Docker container. Docker Compose handles this with volumes, but it's a common issue in other setups.

---

## نسخه فارسی (Persian Version)

# WebBridgeBot

پروژه WebBridgeBot یک ربات تلگرامی است که به عنوان پلی بین تلگرام و مرورگر وب شما عمل می‌کند. این ربات به شما امکان می‌دهد هر فایل ویدیویی، صوتی یا تصویری را به آن ارسال کرده و فوراً یک لینک خصوصی دریافت کنید. این لینک یک پخش‌کننده رسانه مبتنی بر وب را باز می‌کند که محتوا را مستقیماً از ربات استریم کرده و امکان پخش یکپارچه بر روی دستگاه‌هایی مانند تلویزیون، کنسول‌های بازی یا هر مرورگر وب مدرنی را فراهم می‌کند.

---

### ✨ ویژگی‌ها

- **استریم مستقیم رسانه:** ویدیوها، موسیقی و تصاویر را مستقیماً از تلگرام به یک رابط وب استریم کنید، بدون نیاز به دانلود اولیه.
- **پخش فوری:** از وب‌سوکت (WebSocket) برای ارتباط لحظه‌ای بین ربات و پخش‌کننده وب استفاده می‌کند که امکان بارگذاری و کنترل فوری رسانه را فراهم می‌کند.
- **پخش‌کننده وب واکنش‌گرا:** یک رابط وب تمیز و مدرن که بر روی دسکتاپ، تبلت و دستگاه‌های موبایل کار می‌کند. شامل یک ویژوالایزر برای فایل‌های صوتی.
- **مدیریت امن کاربران:** دارای یک سیستم مجوزدهی قوی است. اولین کاربر به عنوان ادمین شناخته شده و می‌تواند به کاربران دیگر مجوز دسترسی یا سطح ادمین بدهد.
- **کش (Cache) کارآمد:** تکه‌های فایل دانلود شده را بر روی دیسک کش می‌کند تا دانلودهای تکراری از تلگرام کاهش یافته و دسترسی سریع‌تری به رسانه‌های پرتکرار فراهم شود.
- **پخش بخشی از محتوا:** از درخواست‌های محدوده HTTP (Range Requests) پشتیبانی می‌کند که به مرورگرها اجازه می‌دهد در فایل‌های رسانه جابجا شوند و محتوا را به طور کارآمد استریم کنند، که برای فایل‌های بزرگ حیاتی است.

### ⚙️ نحوه کار

1.  **ارسال رسانه:** شما یک فایل رسانه (ویدیو، صوت، عکس) را به ربات در یک چت خصوصی ارسال یا فوروارد می‌کنید.
2.  **ایجاد لینک:** ربات فایل را پردازش کرده، یک URL منحصربه‌فرد و امن ایجاد می‌کند و آن را به همراه یک پنل کنترل برای شما ارسال می‌کند.
3.  **باز کردن پخش‌کننده:** شما URL را در هر مرورگری باز می‌کنید. صفحه وب یک اتصال وب‌سوکت به ربات برقرار می‌کند.
4.  **پخش رسانه:** ربات اطلاعات رسانه (مانند نام فایل و نوع) را از طریق وب‌سوکت به پخش‌کننده ارسال می‌کند. سپس پخش‌کننده شروع به استریم محتوای فایل مستقیماً از سرور ربات می‌کند.

### 📋 پیش‌نیازها

- **داکر و داکر کامپوز (Docker & Docker Compose):** برای راه‌اندازی پیشنهادی به صورت کانتینری مورد نیاز است.
- **زبان Go (نسخه 1.21 به بالا):** تنها در صورتی که قصد دارید برنامه را به صورت دستی از سورس کامپایل کنید، لازم است.
- **اطلاعات API تلگرام:**
    - `API ID` و `API Hash`: این مقادیر را از [my.telegram.org](https://my.telegram.org/) دریافت کنید.
    - `توکن ربات (Bot Token)`: یک ربات جدید در [@BotFather](https://t.me/BotFather) در تلگرام ایجاد کرده و توکن آن را دریافت کنید.

### 🔑 مدیریت کاربران و ادمین

این ربات شامل یک سیستم احراز هویت امن برای کنترل دسترسی است.

-   **اولین ادمین:** اولین کاربری که با ربات تعامل می‌کند (با ارسال دستور `/start`) به طور خودکار اختیارات ادمین را دریافت می‌کند.
-   **اختیارات ادمین:** ادمین‌ها اعلان‌هایی برای کاربران جدید دریافت کرده و می‌توانند با دستورات زیر دسترسی‌ها را مدیریت کنند.
-   **مجوزدهی:** تمام کاربران بعدی باید به صورت دستی توسط یک ادمین تأیید شوند تا بتوانند از ربات استفاده کنند. از کاربران غیرمجاز خواسته می‌شود تا درخواست دسترسی دهند.

#### دستورات ادمین

-   `/authorize <user_id>`: به یک کاربر مجوز استفاده از ربات را می‌دهد.
-   `/authorize <user_id> admin`: به یک کاربر مجوز استفاده داده و او را به سطح ادمین ارتقا می‌دهد.
-   `/deauthorize <user_id>`: دسترسی یک کاربر به ربات را لغو می‌کند.
-   `/listusers`: لیستی صفحه‌بندی شده از تمام کاربران و وضعیت آن‌ها را نمایش می‌دهد.
-   `/userinfo <user_id>`: اطلاعات دقیقی در مورد یک کاربر خاص نمایش می‌دهد.

### 🚀 نصب و راه‌اندازی (روش پیشنهادی)

استفاده از داکر کامپوز ساده‌ترین راه برای اجرای WebBridgeBot است.

**۱. کلون کردن مخزن**

```bash
git clone https://github.com/mshafiee/webbridgebot.git
cd webbridgebot
```

**۲. ایجاد فایل `.env`**

فایلی با نام `.env` در ریشه پروژه ایجاد کرده و محتوای زیر را در آن کپی کنید. مقادیر پیش‌فرض را با اطلاعات واقعی خود جایگزین کنید.

```plaintext
# .env - پیکربندی API تلگرام
API_ID=1234567
API_HASH=a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4
BOT_TOKEN=1234567890:ABC-DEF1234ghIkl-zyx57W2v1u123ew11

# پیکربندی سرور وب و URL
# اگر سرور شما به صورت عمومی در دسترس است، localhost را با IP یا دامنه سرور خود جایگزین کنید
BASE_URL=http://localhost:8080
PORT=8080

# (اختیاری) پیکربندی کش
HASH_LENGTH=8
MAX_CACHE_SIZE=10737418240 # 10 گیگابایت به بایت
CACHE_DIRECTORY=.cache
```

**۳. اجرا با داکر کامپوز**

ربات را در پس‌زمینه اجرا کنید:

```bash
docker-compose up -d
```

-   **مشاهده لاگ‌ها:** `docker-compose logs -f`
-   **متوقف کردن ربات:** `docker-compose down`

### 🔧 متغیرهای محیطی

این متغیرها می‌توانند در فایل `.env` یا مستقیماً در محیط شما تنظیم شوند.

| متغیر            | توضیحات                                                        | پیش‌فرض          | الزامی  |
| ----------------- | --------------------------------------------------------------- | ----------------- | -------- |
| `API_ID`          | شناسه API تلگرام شما.                                          | -                 | **بله**  |
| `API_HASH`        | هش API تلگرام شما.                                             | -                 | **بله**  |
| `BOT_TOKEN`       | توکن ربات تلگرام شما.                                          | -                 | **بله**  |
| `BASE_URL`        | URL عمومی که پخش‌کننده وب ربات در آن میزبانی می‌شود.           | `http://localhost:8080` | **بله**  |
| `PORT`            | پورتی که سرور وب بر روی آن اجرا می‌شود.                          | `8080`            | خیر      |
| `HASH_LENGTH`     | طول هش کوتاه استفاده شده در URLهای رسانه.                      | `8`               | خیر      |
| `MAX_CACHE_SIZE`  | حداکثر حجم کش دیسک به بایت.                                     | `10737418240` (10GB) | خیر      |
| `CACHE_DIRECTORY` | دایرکتوری برای ذخیره تکه‌های رسانه کش شده و پایگاه داده.         | `.cache`          | خیر      |
| `DEBUG_MODE`      | برای فعال کردن لاگ‌های کامل، `true` تنظیم کنید.                   | `false`           | خیر      |

### 🤝 مشارکت

از مشارکت شما استقبال می‌کنیم! لطفاً مخزن را فورک کرده، یک شاخه برای ویژگی یا رفع اشکال خود ایجاد کنید و یک درخواست ادغام (pull request) با توضیحات واضح از تغییرات خود ارسال کنید. برای یافتن ایده‌هایی برای کمک، به بخش issues مراجعه کنید.

### 📄 مجوز

پروژه WebBridgeBot تحت **مجوز عمومی همگانی گنو نسخه ۳.۰ (GNU General Public License v3.0)** منتشر شده است. برای جزئیات بیشتر به فایل `LICENSE` مراجعه کنید.

### 🛠️ عیب‌یابی

-   **بررسی متغیرهای محیطی:** اطمینان حاصل کنید که تمام متغیرهای مورد نیاز (`API_ID`, `API_HASH`, `BOT_TOKEN`, `BASE_URL`) به درستی در فایل `.env` شما تنظیم شده‌اند.
-   **بررسی لاگ‌ها:** از دستور `docker-compose logs -f` برای بررسی هرگونه خطا در هنگام راه‌اندازی یا عملکرد استفاده کنید.
-   **مجوزها (Permissions):** مطمئن شوید که دایرکتوری `.cache` دارای مجوزهای نوشتن صحیح برای کانتینر داکر است. داکر کامپوز این مورد را با volumeها مدیریت می‌کند، اما این یک مشکل رایج در تنظیمات دیگر است.
