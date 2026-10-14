import { describe, expect, it } from 'vitest';
import { primaryEndpointError, stubEndpointError, tsigKeyNameError } from './zones';

describe('secondary primary endpoints', () => {
  it.each(['ns1.example.com', 'ns1.example.com.:5353', '192.0.2.1', '192.0.2.1:53', '2001:db8::1', '[2001:db8::1]', '[2001:db8::1]:5353', ' ::1 '])('accepts %s', (endpoint) => {
    expect(primaryEndpointError(endpoint)).toBeNull();
  });

  it.each(['', ':53', '999.1.2.3', 'ns..example', 'ns.example:0', 'ns.example:65536', 'ns.example:http', 'ns example:53', 'tcp://ns.example', '[2001:db8::1', '[192.0.2.1]:53', 'ns.example:'])('rejects %s', (endpoint) => {
    expect(primaryEndpointError(endpoint)).not.toBeNull();
  });

  it('accepts an optional DNS key name and rejects a pasted secret', () => {
    expect(tsigKeyNameError('')).toBeNull();
    expect(tsigKeyNameError('Transfer-Key.Example.')).toBeNull();
    expect(tsigKeyNameError('c2VjcmV0==')).not.toBeNull();
  });
});

describe('stub bootstrap endpoints', () => {
  it.each(['192.0.2.1', 'udp://ns1.example:5353', 'tcp://[2001:db8::1]:5353', 'tcp://ns.example', '::1'])('accepts %s', (endpoint) => {
    expect(stubEndpointError(endpoint)).toBeNull();
  });
  it.each(['', 'tls://ns.example', 'https://ns.example', 'tcp://', 'udp://ns.example:0', 'tcp://user@ns.example', 'udp://ns.example/path'])('rejects %s', (endpoint) => {
    expect(stubEndpointError(endpoint)).not.toBeNull();
  });
});
