"""The command-line parser: defaults, range checks and the settings it builds."""

from __future__ import annotations

import argparse

import pytest

from confclient.cli import MAX_DURATION_S, build_parser, config_from_args


def parse(*argv: str) -> argparse.Namespace:
    return build_parser().parse_args(["--name", "alice", *argv])


def test_defaults() -> None:
    args = parse()
    assert (args.server, args.port, args.fps, args.duration) == ("127.0.0.1", 6100, 15.0, None)
    assert (args.max_send_kbps, args.no_send_cap) == (6000, False)
    assert (args.video_fec_k, args.audio_fec_k, args.jitter_ms) == (4, 2, 60)
    cfg = config_from_args(args)
    assert cfg.media.max_send_kbps == 6000
    assert cfg.media.audio_delay_frames == 3
    assert cfg.video_on
    assert cfg.audio_on


def test_settings_reach_the_client_config() -> None:
    cfg = config_from_args(parse("--no-send-cap", "--jitter-ms", "100", "--no-video", "--video-fec-k", "0"))
    assert cfg.media.max_send_kbps is None  # uncapped
    assert cfg.media.audio_delay_frames == 5
    assert cfg.media.video_fec_k == 0
    assert not cfg.video_on
    assert cfg.audio_on
    assert config_from_args(parse("--max-send-kbps", "1500")).media.max_send_kbps == 1500


@pytest.mark.parametrize("duration", ["0", "2.5", str(MAX_DURATION_S)])
def test_duration_in_range(duration: str) -> None:
    assert parse("--duration", duration).duration == float(duration)


@pytest.mark.parametrize(
    "argv",
    [
        ["--duration", "nan"],
        ["--duration", "-1"],
        ["--duration", "inf"],
        ["--duration", str(MAX_DURATION_S + 1)],
        ["--duration", "soon"],
        ["--port", "0"],
        ["--port", "65536"],
        ["--fps", "nan"],
        ["--jitter-ms", "30"],  # not a multiple of 20
        ["--jitter-ms", "0"],
        ["--max-send-kbps", "0"],
        ["--video-fec-k", "-1"],
        ["--max-send-kbps", "1000", "--no-send-cap"],  # mutually exclusive
    ],
)
def test_rejects(argv: list[str], capsys: pytest.CaptureFixture[str]) -> None:
    with pytest.raises(SystemExit) as exc:
        parse(*argv)
    assert exc.value.code == 2
    assert "error" in capsys.readouterr().err


def test_name_is_required(capsys: pytest.CaptureFixture[str]) -> None:
    with pytest.raises(SystemExit):
        build_parser().parse_args([])
    assert "--name" in capsys.readouterr().err
