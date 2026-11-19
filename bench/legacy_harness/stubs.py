"""Run the original course code in legacy/ without modifying it.

legacy/util.py opens the microphone, the speaker, the camera and pyautogui when it is
imported, and legacy/config.py hard-codes a LAN address. `install()` puts stand-ins
for those device libraries into sys.modules, loads the real legacy config module and
overrides SERVER_IP, then puts legacy/ on sys.path. After that,
`import my_server` / `import my_client` run the original files.

The audio stand-ins model a sound card's clock: an input stream's read(n) returns once
n samples' worth of real time has passed since the previous chunk (as a blocking
PortAudio read does), and an output stream accepts one chunk at a time and plays it at
the real rate, so a caller that writes faster than real time blocks for one chunk
duration per write (as a blocking PortAudio write does once its buffer is full).
"""

from __future__ import annotations

import sys
import threading
import time
import types
from collections.abc import Callable
from pathlib import Path
from typing import Any

LEGACY = Path(__file__).resolve().parents[2] / "legacy"
RATE = 16000

ChunkSource = Callable[[int, float], bytes]  # (chunk index, capture end time) -> PCM bytes
ChunkSink = Callable[[bytes, float], None]  # (PCM bytes, time the device starts playing it)


class InputStream:
    """Blocking microphone: chunk k is available at t0 + (k + 1) * n / RATE."""

    def __init__(self, source: ChunkSource | None) -> None:
        self.source = source
        self.t0: float | None = None
        self.k = 0
        self.lock = threading.Lock()

    def read(self, n: int, exception_on_overflow: bool = True) -> bytes:
        with self.lock:
            now = time.monotonic()
            if self.t0 is None:
                self.t0 = now
            due = self.t0 + (self.k + 1) * n / RATE
            if due > now:
                time.sleep(due - now)
            elif now - due > 1.0:  # far behind: a real device would have overflowed
                self.t0 = now - (self.k + 1) * n / RATE
                due = now
            k = self.k
            self.k += 1
        if self.source is None:
            return bytes(2 * n)
        return self.source(k, due)


class OutputStream:
    """Blocking speaker with room for one chunk: write() returns when the device takes
    the chunk, which is when the previous chunk has finished playing."""

    def __init__(self, sink: ChunkSink | None) -> None:
        self.sink = sink
        self.busy_until = 0.0
        self.lock = threading.Lock()

    def write(self, data: bytes, num_frames: int | None = None, exception_on_underflow: bool = False) -> None:
        with self.lock:
            now = time.monotonic()
            if self.busy_until > now:
                time.sleep(self.busy_until - now)
                now = self.busy_until
            start = max(now, self.busy_until)
            self.busy_until = start + len(data) / 2 / RATE
        if self.sink is not None:
            self.sink(data, start)


class _PyAudio:
    def __init__(self, source: ChunkSource | None, sink: ChunkSink | None) -> None:
        self._source = source
        self._sink = sink

    def open(self, *, input: bool = False, output: bool = False, **_kw: Any) -> Any:
        if input:
            return InputStream(self._source)
        return OutputStream(self._sink)

    def terminate(self) -> None:
        return None


class _VideoCapture:
    def __init__(self, *_a: Any) -> None:
        pass

    def isOpened(self) -> bool:
        return False

    def set(self, *_a: Any) -> bool:
        return False

    def read(self) -> tuple[bool, None]:
        return False, None

    def release(self) -> None:
        return None


def install(
    server_ip: str, audio_source: ChunkSource | None = None, audio_sink: ChunkSink | None = None
) -> None:
    pyaudio = types.ModuleType("pyaudio")
    pyaudio.paInt16 = 8  # type: ignore[attr-defined]
    pyaudio.paContinue = 0  # type: ignore[attr-defined]
    pyaudio.PyAudio = lambda: _PyAudio(audio_source, audio_sink)  # type: ignore[attr-defined]

    cv2 = types.ModuleType("cv2")
    cv2.VideoCapture = _VideoCapture  # type: ignore[attr-defined]
    cv2.CAP_PROP_FRAME_WIDTH = 3  # type: ignore[attr-defined]
    cv2.CAP_PROP_FRAME_HEIGHT = 4  # type: ignore[attr-defined]
    cv2.imshow = lambda *_a: None  # type: ignore[attr-defined]
    cv2.waitKey = lambda *_a: -1  # type: ignore[attr-defined]

    pyautogui = types.ModuleType("pyautogui")
    pyautogui.size = lambda: (1920, 1080)  # type: ignore[attr-defined]

    import PIL

    image_grab = types.ModuleType("PIL.ImageGrab")

    def grab(*_a: Any, **_kw: Any) -> Any:
        raise OSError("screen capture is not available in the benchmark harness")

    image_grab.grab = grab  # type: ignore[attr-defined]
    PIL.ImageGrab = image_grab  # type: ignore[attr-defined]

    sys.modules.update(
        {
            "pyaudio": pyaudio,
            "cv2": cv2,
            "pyautogui": pyautogui,
            "PIL.ImageGrab": image_grab,
        }
    )
    try:
        import tkinter  # noqa: F401  (my_client does `from tkinter import *` and uses nothing)
    except ImportError:
        sys.modules["tkinter"] = types.ModuleType("tkinter")

    sys.path.insert(0, str(LEGACY))
    import config  # type: ignore[import-not-found]  # the real legacy/config.py

    config.SERVER_IP = server_ip
