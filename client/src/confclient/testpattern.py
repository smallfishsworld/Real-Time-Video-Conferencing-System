"""Deterministic synthetic video: scrolling colour bars, a moving zone-plate disc, mild
sensor-like noise and a frame counter, so JPEG frames are about as large as webcam
frames (about 31 KB at 480x480, quality 85) and every frame is distinct."""

from __future__ import annotations

import io

import numpy as np
import numpy.typing as npt
from PIL import Image

Image8 = npt.NDArray[np.uint8]

_BARS = np.array(
    [
        [235, 235, 235],
        [235, 235, 16],
        [16, 235, 235],
        [16, 235, 16],
        [235, 16, 235],
        [235, 16, 16],
        [16, 16, 235],
        [16, 16, 16],
    ],
    dtype=np.int16,
)


class TestPattern:
    """Generates frame n of the pattern; the same (width, height, noise) always gives
    the same pixels for the same n."""

    __test__ = False  # not a pytest test class

    def __init__(self, width: int = 480, height: int = 480, noise: int = 6) -> None:
        self.width = width
        self.height = height
        self.noise = noise
        yy, xx = np.mgrid[0:height, 0:width]
        self._xx = xx.astype(np.float32)
        self._yy = yy.astype(np.float32)
        self._bar_width = width / 8

    def frame(self, n: int) -> Image8:
        w, h = self.width, self.height
        shift = (n * 4) % w
        idx = (((self._xx + shift) % w) // self._bar_width).astype(np.int32)
        img = _BARS[idx]
        cx = w / 2 + 0.3 * w * np.cos(n / 15)
        cy = h / 2 + 0.3 * h * np.sin(n / 15)
        r2 = (self._xx - cx) ** 2 + (self._yy - cy) ** 2
        disc = r2 < (h / 6) ** 2
        zone = (127 + 120 * np.cos(r2 / 180.0 + n / 3)).astype(np.int16)
        img[disc] = zone[disc][:, None]
        if self.noise:
            rng = np.random.default_rng(n)
            img += rng.integers(-self.noise, self.noise + 1, size=img.shape, dtype=np.int16)
        # A 16-bit frame counter as black/white blocks in the top-left corner.
        block = max(4, w // 60)
        for bit in range(16):
            value = 235 if (n >> bit) & 1 else 16
            img[0:block, bit * block : (bit + 1) * block] = value
        out: Image8 = np.clip(img, 0, 255).astype(np.uint8)
        return out


def encode_jpeg(rgb: Image8, quality: int = 85) -> bytes:
    """Encode an RGB frame with Pillow, the encoder the original client used."""
    buf = io.BytesIO()
    Image.fromarray(rgb).save(buf, format="JPEG", quality=quality)
    return buf.getvalue()


def decode_jpeg(data: bytes, fit: tuple[int, int] | None = None) -> Image8:
    """Decode a JPEG. With `fit` (width, height), the decoder may scale the image down by
    1/2, 1/4 or 1/8 as long as it stays at least that large (JPEG draft mode), which is
    far cheaper than decoding at full size and resizing afterwards."""
    with Image.open(io.BytesIO(data)) as im:
        if fit is not None:
            im.draft("RGB", fit)
        return np.asarray(im.convert("RGB"))
