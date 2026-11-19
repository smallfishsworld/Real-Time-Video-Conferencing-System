from __future__ import annotations

import numpy as np
import pytest

from confclient.app import ChatLine
from confclient.testpattern import TestPattern, decode_jpeg, encode_jpeg
from confclient.ui import GridRenderer, Tile, grid_shape


@pytest.mark.parametrize(
    ("n", "shape"),
    [(0, (1, 1)), (1, (1, 1)), (2, (1, 2)), (3, (2, 2)), (4, (2, 2)), (5, (2, 3)), (10, (3, 4))],
)
def test_grid_shape(n: int, shape: tuple[int, int]) -> None:
    assert grid_shape(n) == shape


def test_compose_places_each_tile_in_its_cell() -> None:
    colours = [(250, 0, 0), (0, 250, 0), (0, 0, 250)]
    tiles = [Tile(f"p{i}", np.full((480, 480, 3), c, dtype=np.uint8)) for i, c in enumerate(colours)]
    tiles.append(Tile("waiting", None, "waiting for video"))
    canvas = GridRenderer(tile_width=320, tile_height=240).compose(tiles)
    assert canvas.shape == (480, 640, 3)
    # Centres of the four cells (2 x 2 grid): three colours and an empty cell.
    assert tuple(canvas[120, 160]) == colours[0]
    assert tuple(canvas[120, 480]) == colours[1]
    assert tuple(canvas[360, 160]) == colours[2]
    assert tuple(canvas[360, 480 - 150]) == (24, 24, 24)


def test_square_image_keeps_its_aspect_ratio() -> None:
    tile = Tile("p", np.full((480, 480, 3), 200, dtype=np.uint8))
    canvas = GridRenderer(tile_width=320, tile_height=240).compose([tile])
    row = canvas[120]
    lit = np.flatnonzero(row[:, 0] == 200)
    assert lit.min() == 40  # 240 px wide image centred in a 320 px cell
    assert lit.max() == 279


def test_test_pattern_is_deterministic_and_distinct() -> None:
    pattern = TestPattern(160, 120)
    assert np.array_equal(pattern.frame(3), pattern.frame(3))
    assert not np.array_equal(pattern.frame(3), pattern.frame(4))
    jpeg = encode_jpeg(pattern.frame(0), quality=85)
    assert jpeg[:2] == b"\xff\xd8"
    assert decode_jpeg(jpeg).shape == (120, 160, 3)


def test_decode_for_a_tile_scales_down_in_the_decoder_but_not_below_the_tile() -> None:
    jpeg = encode_jpeg(TestPattern(1280, 720).frame(0), quality=85)
    assert decode_jpeg(jpeg).shape == (720, 1280, 3)
    assert decode_jpeg(jpeg, (320, 240)).shape == (360, 640, 3)  # 1/2: 1/4 would be 180 high
    assert decode_jpeg(jpeg, (160, 90)).shape == (90, 160, 3)  # 1/8


def test_chat_line_has_sender_and_server_timestamp() -> None:
    line = ChatLine(room_id=1, from_id=2, from_name="Zoë", text="hi 你好", server_time_ms=1_700_000_000_123)
    text = line.format()
    assert text.endswith("] Zoë: hi 你好")
    assert text.startswith("[2023-11-")
