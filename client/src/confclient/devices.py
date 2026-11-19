"""Media sources and sinks.

Real devices (OpenCV camera, PyAudio microphone and speaker) are thin adapters whose
libraries are imported only when the device is opened, so the client runs headless
with fake media on machines that have neither. The fakes are deterministic and are
what the tests and benchmarks use.
"""

from __future__ import annotations

import asyncio
import contextlib
import math
import threading
import time
from abc import ABC, abstractmethod
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any

import numpy as np

from .jitter import Frame
from .protocol import AUDIO_CLOCK_RATE, AUDIO_FRAME_SECONDS, AUDIO_SAMPLES_PER_FRAME
from .testpattern import Image8, TestPattern, encode_jpeg

AudioCallback = Callable[[bytes, float], None]  # (big-endian L16 frame, capture time)
PullFrame = Callable[[], Frame]


@dataclass(slots=True)
class CapturedFrame:
    jpeg: bytes
    image: Image8 | None  # RGB pixels for the local preview, if available
    captured: float


class VideoSource(ABC):
    """A camera-like source; capture() blocks until the next frame is due."""

    fps: float

    @abstractmethod
    def capture(self) -> CapturedFrame | None: ...

    def close(self) -> None:
        return None


class AudioSource(ABC):
    """Delivers one 20 ms frame per call of the callback, in the event loop thread."""

    @abstractmethod
    def start(self, loop: asyncio.AbstractEventLoop, on_frame: AudioCallback) -> None: ...

    @abstractmethod
    def stop(self) -> None: ...


class AudioSink(ABC):
    """Plays audio by pulling one frame every 20 ms."""

    @abstractmethod
    def start(self, pull: PullFrame) -> None: ...

    @abstractmethod
    def stop(self) -> None: ...


class _Pacer:
    """Deadline-based pacing that does not drift: tick n is due at t0 + n * period."""

    def __init__(self, period: float) -> None:
        self.period = period
        self.t0 = time.monotonic()
        self.n = 0

    def wait(self) -> float:
        self.n += 1
        due = self.t0 + self.n * self.period
        delay = due - time.monotonic()
        if delay > 0:
            time.sleep(delay)
        elif delay < -10 * self.period:  # fell far behind: restart the schedule
            self.t0 = time.monotonic()
            self.n = 0
        return time.monotonic()


class FakeCamera(VideoSource):
    """The synthetic test pattern, pre-encoded once as a loop of `loop_frames` frames so
    that sending costs no encoding time."""

    def __init__(
        self, width: int = 480, height: int = 480, fps: float = 15.0, quality: int = 85, loop_frames: int = 30
    ) -> None:
        self.fps = fps
        pattern = TestPattern(width, height)
        self._frames: list[tuple[bytes, Image8]] = []
        for n in range(loop_frames):
            img = pattern.frame(n)
            self._frames.append((encode_jpeg(img, quality), img))
        self._pacer = _Pacer(1.0 / fps)
        self._n = 0

    def capture(self) -> CapturedFrame:
        t = self._pacer.wait()
        jpeg, img = self._frames[self._n % len(self._frames)]
        self._n += 1
        return CapturedFrame(jpeg, img, t)


class OpenCVCamera(VideoSource):
    """A real camera through OpenCV (`pip install confclient[video]`)."""

    def __init__(
        self, index: int = 0, width: int = 480, height: int = 480, fps: float = 15.0, quality: int = 85
    ):
        import cv2

        self._cv2: Any = cv2
        self._cap = cv2.VideoCapture(index)
        if not self._cap.isOpened():
            raise RuntimeError(f"cannot open camera {index}")
        self._cap.set(cv2.CAP_PROP_FRAME_WIDTH, width)
        self._cap.set(cv2.CAP_PROP_FRAME_HEIGHT, height)
        self.fps = fps
        self._quality = quality
        self._pacer = _Pacer(1.0 / fps)

    def capture(self) -> CapturedFrame | None:
        self._pacer.wait()
        ok, bgr = self._cap.read()
        if not ok:
            return None
        t = time.monotonic()
        ok, buf = self._cv2.imencode(".jpg", bgr, [self._cv2.IMWRITE_JPEG_QUALITY, self._quality])
        if not ok:
            return None
        return CapturedFrame(buf.tobytes(), bgr[:, :, ::-1], t)

    def close(self) -> None:
        self._cap.release()


def tone(freq: float, amplitude: int, n0: int) -> Frame:
    """320 samples of a sine tone starting at absolute sample index n0."""
    n = np.arange(n0, n0 + AUDIO_SAMPLES_PER_FRAME)
    return (amplitude * np.sin(2 * math.pi * freq * n / AUDIO_CLOCK_RATE)).astype(np.int16)


class FakeMicrophone(AudioSource):
    """A sine tone, one 20 ms frame per tick, paced by the event loop's clock."""

    def __init__(self, freq: float = 440.0, amplitude: int = 3000) -> None:
        self.freq = freq
        self.amplitude = amplitude
        self._task: asyncio.Task[None] | None = None
        self.frames = 0

    def make_frame(self, index: int) -> Frame:
        return tone(self.freq, self.amplitude, index * AUDIO_SAMPLES_PER_FRAME)

    def start(self, loop: asyncio.AbstractEventLoop, on_frame: AudioCallback) -> None:
        self._task = loop.create_task(self._run(on_frame), name="fake-mic")

    async def _run(self, on_frame: AudioCallback) -> None:
        t0 = time.monotonic()
        i = 0
        while True:
            i += 1
            delay = t0 + i * AUDIO_FRAME_SECONDS - time.monotonic()
            if delay > 0:
                await asyncio.sleep(delay)
            frame = self.make_frame(self.frames)
            self.frames += 1
            on_frame(frame.astype(">i2").tobytes(), time.monotonic())

    def stop(self) -> None:
        if self._task is not None:
            self._task.cancel()
            self._task = None


class FakeSpeaker(AudioSink):
    """Pulls a frame every 20 ms on its own thread, like a sound card would, and keeps
    counters instead of producing sound."""

    def __init__(self) -> None:
        self._thread: threading.Thread | None = None
        self._stop = threading.Event()
        self.frames = 0
        self.nonsilent = 0

    def start(self, pull: PullFrame) -> None:
        self._stop.clear()
        self._thread = threading.Thread(target=self._run, args=(pull,), name="fake-speaker", daemon=True)
        self._thread.start()

    def _run(self, pull: PullFrame) -> None:
        pacer = _Pacer(AUDIO_FRAME_SECONDS)
        while not self._stop.is_set():
            pacer.wait()
            frame = pull()
            self.frames += 1
            if frame.any():
                self.nonsilent += 1

    def stop(self) -> None:
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=1)
            self._thread = None


class PyAudioMicrophone(AudioSource):
    """A real microphone through PyAudio (`pip install confclient[audio]`)."""

    def __init__(self) -> None:
        import pyaudio

        self._pa: Any = pyaudio.PyAudio()
        self._stream: Any = None

    def start(self, loop: asyncio.AbstractEventLoop, on_frame: AudioCallback) -> None:
        import pyaudio

        def callback(data: bytes, frames: int, info: Any, status: int) -> tuple[None, int]:
            pcm = np.frombuffer(data, dtype=np.int16).astype(">i2").tobytes()
            loop.call_soon_threadsafe(on_frame, pcm, time.monotonic())
            return None, pyaudio.paContinue

        self._stream = self._pa.open(
            format=pyaudio.paInt16,
            channels=1,
            rate=AUDIO_CLOCK_RATE,
            input=True,
            frames_per_buffer=AUDIO_SAMPLES_PER_FRAME,
            stream_callback=callback,
        )
        self._stream.start_stream()

    def stop(self) -> None:
        if self._stream is not None:
            with contextlib.suppress(OSError):
                self._stream.stop_stream()
                self._stream.close()
            self._stream = None


class PyAudioSpeaker(AudioSink):
    """A real speaker through PyAudio; the sound card's callback pulls each frame, so
    the device clock drives playout."""

    def __init__(self) -> None:
        import pyaudio

        self._pa: Any = pyaudio.PyAudio()
        self._stream: Any = None

    def start(self, pull: PullFrame) -> None:
        import pyaudio

        def callback(_in: bytes | None, frames: int, info: Any, status: int) -> tuple[bytes, int]:
            return pull().tobytes(), pyaudio.paContinue

        self._stream = self._pa.open(
            format=pyaudio.paInt16,
            channels=1,
            rate=AUDIO_CLOCK_RATE,
            output=True,
            frames_per_buffer=AUDIO_SAMPLES_PER_FRAME,
            stream_callback=callback,
        )
        self._stream.start_stream()

    def stop(self) -> None:
        if self._stream is not None:
            with contextlib.suppress(OSError):
                self._stream.stop_stream()
                self._stream.close()
            self._stream = None
