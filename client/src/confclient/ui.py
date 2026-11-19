"""Split-screen view: every participant (and the local preview) is a tile in one
window. Composition uses numpy and Pillow only; the window itself needs OpenCV."""

from __future__ import annotations

import math
from dataclasses import dataclass
from typing import Any

import numpy as np
from PIL import Image, ImageDraw

from .testpattern import Image8

_BACKGROUND = (24, 24, 24)
_LABEL_BG = (0, 0, 0)
_LABEL_FG = (255, 255, 255)


@dataclass(slots=True)
class Tile:
    name: str
    image: Image8 | None  # RGB, any size; None shows `status` instead
    status: str = ""


def grid_shape(n: int) -> tuple[int, int]:
    """(rows, cols) of the most square grid that holds n tiles."""
    if n <= 0:
        return 1, 1
    cols = math.ceil(math.sqrt(n))
    rows = math.ceil(n / cols)
    return rows, cols


class GridRenderer:
    """Lays tiles out row by row, scaling each image to fit its cell while keeping its
    aspect ratio."""

    def __init__(self, tile_width: int = 320, tile_height: int = 240) -> None:
        self.tile_width = tile_width
        self.tile_height = tile_height

    def compose(self, tiles: list[Tile]) -> Image8:
        rows, cols = grid_shape(len(tiles))
        canvas = Image.new("RGB", (cols * self.tile_width, rows * self.tile_height), _BACKGROUND)
        draw = ImageDraw.Draw(canvas)
        for i, tile in enumerate(tiles):
            x0 = (i % cols) * self.tile_width
            y0 = (i // cols) * self.tile_height
            if tile.image is not None:
                img = Image.fromarray(tile.image)
                scale = min(self.tile_width / img.width, self.tile_height / img.height)
                size = (max(1, int(img.width * scale)), max(1, int(img.height * scale)))
                img = img.resize(size, Image.Resampling.BILINEAR)
                canvas.paste(
                    img, (x0 + (self.tile_width - size[0]) // 2, y0 + (self.tile_height - size[1]) // 2)
                )
            elif tile.status:
                draw.text((x0 + 10, y0 + self.tile_height // 2), tile.status, fill=_LABEL_FG)
            label = tile.name
            box = draw.textbbox((0, 0), label)
            draw.rectangle((x0, y0, x0 + box[2] + 8, y0 + box[3] + 6), fill=_LABEL_BG)
            draw.text((x0 + 4, y0 + 2), label, fill=_LABEL_FG)
        return np.asarray(canvas)


class OpenCVWindow:
    """One OpenCV window. Call show() from the main thread (required on macOS)."""

    def __init__(self, title: str) -> None:
        import cv2

        self._cv2: Any = cv2
        self.title = title
        cv2.namedWindow(title, cv2.WINDOW_NORMAL)

    def show(self, canvas: Image8) -> int:
        self._cv2.imshow(self.title, canvas[:, :, ::-1])
        return int(self._cv2.waitKey(1))

    def close(self) -> None:
        self._cv2.destroyWindow(self.title)
        self._cv2.waitKey(1)
