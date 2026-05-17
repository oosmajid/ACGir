import os
import queue
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import urllib.parse
import zipfile
from pathlib import Path
from tkinter import filedialog, messagebox, ttk
import tkinter as tk

import requests


APP_TITLE = "ACcrawler"
CHUNK_SIZE = 1024 * 1024


def get_ffmpeg_executable():
    try:
        import imageio_ffmpeg

        return imageio_ffmpeg.get_ffmpeg_exe()
    except Exception:
        return os.environ.get("FFMPEG_BINARY", "ffmpeg")


def build_download_url(url):
    parsed = urllib.parse.urlparse(url.strip())
    if not parsed.scheme or not parsed.netloc:
        raise ValueError("لینک وارد شده معتبر نیست.")

    path = parsed.path if parsed.path.endswith("/") else parsed.path + "/"
    new_path = path + "output/lecture.zip"

    query_params = urllib.parse.parse_qs(parsed.query)
    output_query = [("download", "zip")]
    session = query_params.get("session", [""])[0]
    if session:
        output_query.append(("session", session))

    return urllib.parse.urlunparse(
        (
            parsed.scheme,
            parsed.netloc,
            new_path,
            "",
            urllib.parse.urlencode(output_query),
            "",
        )
    )


def safe_filename(filename, fallback):
    filename = urllib.parse.unquote(Path(filename).name).strip()
    if not filename:
        filename = fallback

    filename = re.sub(r'[<>:"/\\|?*\x00-\x1f]', "_", filename)
    filename = re.sub(r"\s+", " ", filename).strip(" .")
    if not filename:
        filename = fallback

    stem, suffix = os.path.splitext(filename)
    if len(filename) > 140:
        filename = stem[: 140 - len(suffix)] + suffix

    return filename


def unique_path(path):
    path = Path(path)
    if not path.exists():
        return path

    for index in range(2, 1000):
        candidate = path.with_name(f"{path.stem} ({index}){path.suffix}")
        if not candidate.exists():
            return candidate

    raise ValueError("امکان ساخت نام فایل خروجی یکتا وجود ندارد.")


def extract_media_file(zip_path, temp_dir):
    with zipfile.ZipFile(zip_path, "r") as archive:
        media_infos = [
            info
            for info in archive.infolist()
            if not info.is_dir() and info.filename.lower().endswith((".flv", ".mp4"))
        ]
        if not media_infos:
            raise ValueError("هیچ فایل صوتی/تصویری در این کلاس یافت نشد.")

        media_info = max(media_infos, key=lambda item: item.file_size)
        media_name = safe_filename(media_info.filename, "lecture.mp4")
        extracted_path = Path(temp_dir) / media_name

        with archive.open(media_info) as source, extracted_path.open("wb") as target:
            shutil.copyfileobj(source, target)

        return extracted_path


def convert_to_mp3(source_path, target_path):
    ffmpeg_exe = get_ffmpeg_executable()
    command = [
        ffmpeg_exe,
        "-y",
        "-i",
        str(source_path),
        "-vn",
        "-acodec",
        "libmp3lame",
        "-q:a",
        "2",
        str(target_path),
    ]

    run_options = {"check": True, "capture_output": True, "text": True}
    if os.name == "nt":
        run_options["creationflags"] = subprocess.CREATE_NO_WINDOW

    try:
        subprocess.run(command, **run_options)
    except FileNotFoundError as exc:
        raise ValueError(
            "ابزار ffmpeg پیدا نشد. نسخه پرتابل را کامل بسازید و کل پوشه خروجی را کنار هم نگه دارید."
        ) from exc
    except subprocess.CalledProcessError as exc:
        error_output = exc.stderr or exc.stdout or str(exc)
        raise ValueError(f"خطا در تبدیل به MP3:\n{error_output}") from exc


def download_and_convert(url, output_dir, report):
    temp_dir = Path(tempfile.mkdtemp(prefix="accrawler-"))
    output_dir = Path(output_dir)

    try:
        zip_url = build_download_url(url)
        zip_path = temp_dir / "lecture.zip"

        report(0.05, "در حال اتصال به سرور...")
        with requests.get(zip_url, stream=True, timeout=(15, 60)) as response:
            if response.status_code != 200:
                raise ValueError(
                    f"خطا در ارتباط با سرور. وضعیت: {response.status_code}. ممکن است لینک یا session منقضی شده باشد."
                )

            total_size = int(response.headers.get("content-length", 0) or 0)
            downloaded = 0
            with zip_path.open("wb") as target:
                for chunk in response.iter_content(CHUNK_SIZE):
                    if not chunk:
                        continue

                    target.write(chunk)
                    downloaded += len(chunk)

                    if total_size:
                        percent = min(downloaded / total_size, 1)
                        report(0.05 + percent * 0.75, "در حال دانلود کلاس...")
                    else:
                        report(0.35, "در حال دانلود کلاس...")

        report(0.84, "در حال استخراج فایل اصلی...")
        extracted_path = extract_media_file(zip_path, temp_dir)

        output_dir.mkdir(parents=True, exist_ok=True)
        original_path = unique_path(output_dir / safe_filename(extracted_path.name, "lecture.mp4"))
        shutil.copy2(extracted_path, original_path)

        report(0.92, "در حال ساخت فایل MP3...")
        mp3_path = unique_path(output_dir / f"{original_path.stem}.mp3")
        convert_to_mp3(original_path, mp3_path)

        report(1.0, "انجام شد.")
        return original_path, mp3_path
    finally:
        shutil.rmtree(temp_dir, ignore_errors=True)


class ACcrawlerApp(tk.Tk):
    def __init__(self):
        super().__init__()

        self.title(APP_TITLE)
        self.geometry("720x430")
        self.minsize(580, 390)

        default_output = Path.home() / "Downloads" / "ACcrawler"
        self.url_var = tk.StringVar()
        self.output_dir_var = tk.StringVar(value=str(default_output))
        self.status_var = tk.StringVar(value="آماده")
        self.events = queue.Queue()

        self._build_ui()
        self.after(100, self._handle_worker_events)

    def _build_ui(self):
        style = ttk.Style(self)
        if "vista" in style.theme_names():
            style.theme_use("vista")

        root = ttk.Frame(self, padding=18)
        root.pack(fill="both", expand=True)
        root.columnconfigure(0, weight=1)

        title = ttk.Label(root, text="دانلودر کلاس Adobe Connect", font=("Segoe UI", 16, "bold"))
        title.grid(row=0, column=0, sticky="e", pady=(0, 18))

        ttk.Label(root, text="لینک کلاس").grid(row=1, column=0, sticky="e")
        url_entry = ttk.Entry(root, textvariable=self.url_var, justify="left")
        url_entry.grid(row=2, column=0, sticky="ew", pady=(6, 14))
        url_entry.focus_set()

        ttk.Label(root, text="پوشه ذخیره").grid(row=3, column=0, sticky="e")
        output_row = ttk.Frame(root)
        output_row.grid(row=4, column=0, sticky="ew", pady=(6, 14))
        output_row.columnconfigure(0, weight=1)

        output_entry = ttk.Entry(output_row, textvariable=self.output_dir_var, state="readonly")
        output_entry.grid(row=0, column=0, sticky="ew", padx=(0, 8))

        browse_button = ttk.Button(output_row, text="انتخاب", command=self._choose_output_dir)
        browse_button.grid(row=0, column=1)

        self.progress = ttk.Progressbar(root, mode="determinate", maximum=100)
        self.progress.grid(row=5, column=0, sticky="ew", pady=(4, 8))

        self.status_label = ttk.Label(root, textvariable=self.status_var, anchor="e")
        self.status_label.grid(row=6, column=0, sticky="ew", pady=(0, 14))

        self.result_text = tk.Text(root, height=5, wrap="word", state="disabled")
        self.result_text.grid(row=7, column=0, sticky="nsew", pady=(0, 14))
        root.rowconfigure(7, weight=1)

        button_row = ttk.Frame(root)
        button_row.grid(row=8, column=0, sticky="ew")
        button_row.columnconfigure(0, weight=1)

        self.open_folder_button = ttk.Button(
            button_row,
            text="باز کردن پوشه",
            command=self._open_output_dir,
            state="disabled",
        )
        self.open_folder_button.grid(row=0, column=0, sticky="w")

        self.start_button = ttk.Button(button_row, text="شروع", command=self._start)
        self.start_button.grid(row=0, column=1, sticky="e")

    def _choose_output_dir(self):
        selected_dir = filedialog.askdirectory(initialdir=self.output_dir_var.get())
        if selected_dir:
            self.output_dir_var.set(selected_dir)

    def _set_result(self, text):
        self.result_text.configure(state="normal")
        self.result_text.delete("1.0", "end")
        self.result_text.insert("1.0", text)
        self.result_text.configure(state="disabled")

    def _set_busy(self, busy):
        state = "disabled" if busy else "normal"
        self.start_button.configure(state=state)

    def _start(self):
        url = self.url_var.get().strip()
        output_dir = self.output_dir_var.get().strip()

        if not url:
            messagebox.showerror(APP_TITLE, "لینک کلاس را وارد کنید.")
            return

        if not output_dir:
            messagebox.showerror(APP_TITLE, "پوشه ذخیره را انتخاب کنید.")
            return

        self._set_busy(True)
        self.open_folder_button.configure(state="disabled")
        self.progress["value"] = 0
        self.status_var.set("شروع شد...")
        self._set_result("")

        worker = threading.Thread(
            target=self._worker,
            args=(url, output_dir),
            daemon=True,
        )
        worker.start()

    def _worker(self, url, output_dir):
        def report(progress, status):
            self.events.put(("progress", progress, status))

        try:
            original_path, mp3_path = download_and_convert(url, output_dir, report)
            self.events.put(("done", original_path, mp3_path))
        except Exception as exc:
            self.events.put(("error", str(exc)))

    def _handle_worker_events(self):
        try:
            while True:
                event = self.events.get_nowait()
                kind = event[0]

                if kind == "progress":
                    _, progress, status = event
                    self.progress["value"] = int(progress * 100)
                    self.status_var.set(status)
                elif kind == "done":
                    _, original_path, mp3_path = event
                    self.progress["value"] = 100
                    self.status_var.set("فایل‌ها آماده هستند.")
                    self._set_busy(False)
                    self.open_folder_button.configure(state="normal")
                    self._set_result(
                        f"فایل اصلی:\n{original_path}\n\nفایل MP3:\n{mp3_path}"
                    )
                    messagebox.showinfo(APP_TITLE, "دانلود و تبدیل کامل شد.")
                elif kind == "error":
                    _, error = event
                    self.status_var.set("خطا")
                    self._set_busy(False)
                    self.open_folder_button.configure(state="normal")
                    self._set_result(error)
                    messagebox.showerror(APP_TITLE, error)
        except queue.Empty:
            pass

        self.after(100, self._handle_worker_events)

    def _open_output_dir(self):
        output_dir = Path(self.output_dir_var.get())
        output_dir.mkdir(parents=True, exist_ok=True)

        if os.name == "nt":
            os.startfile(str(output_dir))
        elif sys.platform == "darwin":
            subprocess.run(["open", str(output_dir)], check=False)
        else:
            subprocess.run(["xdg-open", str(output_dir)], check=False)


if __name__ == "__main__":
    app = ACcrawlerApp()
    app.mainloop()
