import http.server, os, json

NOTES_FILE = '/data/notes.json'


class Handler(http.server.BaseHTTPRequestHandler):
    def _notes(self):
        return json.load(open(NOTES_FILE)) if os.path.exists(NOTES_FILE) else []

    def do_GET(self):
        body = json.dumps({
            'app': os.environ.get('APP_TITLE', 'demo'),
            'message': os.environ.get('APP_MESSAGE', ''),
            'token': os.environ.get('APP_TOKEN', ''),
            'notes': self._notes()
        })
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(body.encode())

    def do_POST(self):
        data = self.rfile.read(int(self.headers['Content-Length'])).decode()
        notes = self._notes()
        notes.append({'id': len(notes) + 1, 'text': data})
        json.dump(notes, open(NOTES_FILE, 'w'))
        self.send_response(201)
        self.end_headers()


http.server.HTTPServer(('', 8080), Handler).serve_forever()
