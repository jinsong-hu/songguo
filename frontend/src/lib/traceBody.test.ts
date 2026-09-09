import { describe, expect, it } from 'vitest';
import { MAX_VALUE_CHARS, displayBody } from './traceBody';

/** A base64-ish blob of `n` characters, like a captured image payload. */
const blob = (n: number) => 'AmO4kSC0oeWQMKKMQWeo'.repeat(Math.ceil(n / 20)).slice(0, n);

describe('displayBody', () => {
  it('pretty-prints JSON, as the old prettyBody did', () => {
    expect(displayBody('{"a":1,"b":[2]}')).toEqual({
      text: '{\n  "a": 1,\n  "b": [\n    2\n  ]\n}',
      elided: 0,
    });
  });

  it('returns a non-JSON body verbatim', () => {
    const sse = 'event: message_stop\ndata: {"type":"message_stop"}\n\n';
    expect(displayBody(sse)).toEqual({ text: sse, elided: 0 });
  });

  it('leaves an ordinary prompt whole', () => {
    // The limit exists for blobs; a long prompt must still read in full.
    const prompt = blob(MAX_VALUE_CHARS);
    const out = displayBody(JSON.stringify({ prompt }));
    expect(out.elided).toBe(0);
    expect(out.text).toContain(prompt);
  });

  it('shortens the image payload that made the pane render blank', () => {
    // 3 MiB of base64 on one line is ~21.8M CSS px, past the 2^24 px at which
    // Chrome stops painting the run.
    const b64 = blob(3 * 1024 * 1024);
    const out = displayBody(JSON.stringify({ created: 1, data: [{ b64_json: b64 }] }));

    expect(out.elided).toBe(b64.length - MAX_VALUE_CHARS);
    expect(out.text.length).toBeLessThan(4000);
    // The structure around the blob survives — that is the whole point.
    expect(out.text).toContain('"created": 1');
    expect(out.text).toContain('"b64_json"');
    expect(out.text).toContain('more characters');
    // And the kept prefix is the real prefix, not a placeholder.
    expect(out.text).toContain(b64.slice(0, 200));
  });

  it('counts every shortened value, not just the first', () => {
    const a = blob(5000);
    const c = blob(9000);
    const out = displayBody(JSON.stringify({ a, b: 'short', c }));
    expect(out.elided).toBe(a.length + c.length - 2 * MAX_VALUE_CHARS);
  });

  it('shortens a blob inside a non-JSON stream line, keeping the other lines', () => {
    const body = `event: image\ndata: ${blob(50_000)}\nevent: done\n`;
    const out = displayBody(body);
    expect(out.elided).toBeGreaterThan(0);
    expect(out.text).toContain('event: image');
    expect(out.text).toContain('event: done');
    expect(out.text.split('\n')).toHaveLength(4);
  });

  it('shortens a base64 binary body without trying to parse it', () => {
    const out = displayBody(blob(3 * 1024 * 1024), true);
    expect(out.elided).toBe(3 * 1024 * 1024 - MAX_VALUE_CHARS);
    expect(out.text.length).toBeLessThan(2200);
  });

  it('does not report elision for a body it left alone', () => {
    // A false count would put a chip on a pane showing everything.
    for (const body of ['', '{}', 'plain text', JSON.stringify({ x: blob(100) })]) {
      expect(displayBody(body).elided).toBe(0);
    }
  });

  it('keeps every rendered line under the width Chrome will paint', () => {
    // The invariant the fix exists for: ~6.94px per monospace char at 11.5px,
    // so a line must stay far below 2^24 px.
    const out = displayBody(JSON.stringify({ b64_json: blob(4 * 1024 * 1024) }));
    for (const line of out.text.split('\n')) {
      expect(line.length * 6.94).toBeLessThan(16_777_216);
    }
  });
});
