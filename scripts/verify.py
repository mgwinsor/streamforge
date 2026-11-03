#!/usr/bin/env python3
"""Black-box acceptance checks against an already running Compose API."""
import json
import os
import pathlib
import subprocess
import tempfile
import time
import urllib.request

BASE = os.environ.get('BASE_URL', 'http://localhost:8080')

def get(path):
    with urllib.request.urlopen(BASE + path, timeout=10) as response:
        return json.load(response)

def upload(path, title):
    result = subprocess.run(['curl', '-sS', '-w', '\n%{http_code}', '-F', f'file=@{path}', '-F', f'title={title}', BASE + '/api/videos'], capture_output=True, text=True, check=True)
    body, code = result.stdout.rsplit('\n', 1)
    assert code == '202', (code, body)
    video = json.loads(body)
    assert video['status'] == 'queued', video
    return video['id']

def wait(video_id, status):
    end = time.monotonic() + 120
    while time.monotonic() < end:
        video = get('/api/videos/' + video_id)
        if video['status'] in ('ready', 'failed'):
            assert video['status'] == status, video
            return video
        time.sleep(.3)
    raise AssertionError('Job did not finish: ' + video_id)

assert get('/healthz')['status'] == 'ok'
assert get('/readyz')['status'] == 'ok'
for name in ('demo', 'silent'):
    started = time.monotonic()
    video_id = upload('samples/' + name + '.mp4', name)
    print(f'{name}: HTTP 202 after {time.monotonic()-started:.3f}s; id={video_id}', flush=True)
    video = wait(video_id, 'ready')
    assert all(video[k] for k in ('created_at', 'started_at', 'finished_at', 'playback_url'))
    url = BASE + video['playback_url']
    master = urllib.request.urlopen(url).read().decode()
    assert '360p/index.m3u8' in master and '720p/index.m3u8' in master
    for rendition, height in [('360p', 360), ('720p', 720)]:
        playlist_url = url.replace('master.m3u8', rendition + '/index.m3u8')
        playlist = urllib.request.urlopen(playlist_url).read().decode()
        assert '#EXT-X-ENDLIST' in playlist
        segments = [line for line in playlist.splitlines() if line and not line.startswith('#')]
        assert len(segments) >= 3, segments
        for segment in segments:
            data = urllib.request.urlopen(playlist_url.rsplit('/', 1)[0] + '/' + segment).read()
            assert len(data) > 0
        info = json.loads(subprocess.check_output(['ffprobe', '-v', 'error', '-show_streams', '-of', 'json', playlist_url]))
        streams = info['streams']
        stream = next(s for s in streams if s['codec_type'] == 'video')
        assert stream['codec_name'] == 'h264' and stream['height'] == height and stream['width'] % 2 == 0
        assert any(s['codec_type'] == 'audio' for s in streams) == (name == 'demo')
        if name == 'demo':
            assert next(s for s in streams if s['codec_type'] == 'audio')['codec_name'] == 'aac'
        subprocess.run(['ffmpeg', '-v', 'error', '-i', playlist_url, '-f', 'null', '-'], check=True, timeout=30)
    print(name + ': both HLS renditions fetched, inspected and decoded', flush=True)

with tempfile.TemporaryDirectory() as temp:
    corrupt = pathlib.Path(temp) / 'bad.mp4'
    corrupt.write_text('not a video')
    video = wait(upload(corrupt, 'Corrupt input'), 'failed')
    assert video['failure_message'] and 'playback_url' not in video
    assert '/data/' not in json.dumps(video) and 'ffmpeg version' not in json.dumps(video)
    print('corrupt input: failed with sanitized message', flush=True)
    large = pathlib.Path(temp) / 'large.mp4'
    with large.open('wb') as file:
        file.truncate(100 * 1024 * 1024 + 1)
    result = subprocess.check_output(['curl', '-sS', '-w', '\n%{http_code}', '-F', f'file=@{large}', BASE + '/api/videos'], text=True)
    body, code = result.rsplit('\n', 1)
    assert code == '413' and json.loads(body)['error']['code'] == 'upload_too_large'
    print('oversized upload: HTTP 413', flush=True)
print('Compose API acceptance checks passed. Browser playback is a separate check.')
