import http.server, os, json, ssl, urllib.request
from datetime import datetime, timedelta, timezone

NOTES_FILE = '/data/notes.json'
TOKEN_PATH = '/var/run/secrets/kubernetes.io/serviceaccount/token'
CA_PATH = '/var/run/secrets/kubernetes.io/serviceaccount/ca.crt'
API_HOST = 'https://kubernetes.default.svc'
NAMESPACE = os.environ.get('NAMESPACE', 'shift-week-demo')


def _read_noteconfig():
    try:
        token = open(TOKEN_PATH).read()
        url = f'{API_HOST}/apis/shift-week.example.com/v1/namespaces/{NAMESPACE}/noteconfigs/default'
        req = urllib.request.Request(url, headers={'Authorization': f'Bearer {token}'})
        ctx = ssl.create_default_context(cafile=CA_PATH)
        resp = json.load(urllib.request.urlopen(req, context=ctx))
        return resp.get('spec', {})
    except Exception:
        return {'maxNotes': 100, 'retentionDays': 30}


def _load_notes():
    return json.load(open(NOTES_FILE)) if os.path.exists(NOTES_FILE) else []


def _save_notes(notes):
    json.dump(notes, open(NOTES_FILE, 'w'))


def _purge_expired(notes):
    now = datetime.now(timezone.utc)
    live = [n for n in notes if datetime.fromisoformat(n['expiresAt']) > now]
    if len(live) != len(notes):
        _save_notes(live)
    return live


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        config = _read_noteconfig()
        notes = _purge_expired(_load_notes())
        body = json.dumps({
            'app': os.environ.get('APP_TITLE', 'demo'),
            'message': os.environ.get('APP_MESSAGE', ''),
            'token': os.environ.get('APP_TOKEN', ''),
            'config': config,
            'notes': notes
        })
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(body.encode())

    def do_POST(self):
        config = _read_noteconfig()
        notes = _purge_expired(_load_notes())
        max_notes = config.get('maxNotes', 100)
        if len(notes) >= max_notes:
            self.send_response(409)
            self.send_header('Content-Type', 'application/json')
            self.end_headers()
            self.wfile.write(json.dumps({'error': f'max notes ({max_notes}) reached'}).encode())
            return
        data = self.rfile.read(int(self.headers['Content-Length'])).decode()
        retention = config.get('retentionDays', 30)
        now = datetime.now(timezone.utc)
        expires = now + timedelta(days=retention)
        notes.append({
            'id': len(notes) + 1,
            'text': data,
            'createdAt': now.isoformat(),
            'expiresAt': expires.isoformat()
        })
        _save_notes(notes)
        self.send_response(201)
        self.end_headers()


http.server.HTTPServer(('', 8080), Handler).serve_forever()
