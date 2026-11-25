import { detectValueLanguage, VALUE_LANGUAGES, ValueLanguage } from '../value-language';

const format = (language: ValueLanguage, text: string) => VALUE_LANGUAGES[language].format(text);

describe('detectValueLanguage', () => {
  it.each([
    ['config.json', ValueLanguage.json],
    ['values.YAML', ValueLanguage.yaml],
    ['app.yml', ValueLanguage.yaml],
    ['settings.xml', ValueLanguage.xml],
    ['php.ini', ValueLanguage.ini],
    ['my.cnf', ValueLanguage.ini],
    ['config.toml', ValueLanguage.toml],
    ['app.properties', ValueLanguage.properties],
    ['.env', ValueLanguage.properties],
    ['entrypoint.sh', ValueLanguage.shell],
  ])('detects %s from the key extension', (key, expected) => {
    expect(detectValueLanguage(key, 'anything')).toBe(expected);
  });

  it.each([
    ['a JSON object', '{"user": "admin", "password": "hunter2"}', ValueLanguage.json],
    ['a multi-line JSON object being typed', '{\n  "user": ', ValueLanguage.json],
    ['a JSON array', '[1, 2, 3]', ValueLanguage.json],
    ['a JSON array of strings', '["a", "b"]', ValueLanguage.json],
    ['an XML document', '<?xml version="1.0"?><settings/>', ValueLanguage.xml],
    ['a single XML element', '<user name="a">b</user>', ValueLanguage.xml],
    ['multi-line XML', '<settings>\n  <a/>\n</settings>', ValueLanguage.xml],
    ['an INI file', '[database]\nuser = admin\npassword = secret', ValueLanguage.ini],
    ['INI with leading comments', '; comment\n[server]\nport=80', ValueLanguage.ini],
    ['TOML array tables', '[[servers]]\nname = "a"\n[[servers]]\nname = "b"', ValueLanguage.toml],
    ['TOML multi-line strings', '[server]\nmotd = """\nhi\n"""', ValueLanguage.toml],
    ['a dotenv file', 'DB_USER=admin\n# comment\nexport DB_PASS=secret', ValueLanguage.properties],
    ['a YAML mapping', 'user: admin\npassword: secret', ValueLanguage.yaml],
    ['a YAML list', '- one\n- two', ValueLanguage.yaml],
    ['a YAML stream', '---\na: 1\n---\nb: 2', ValueLanguage.yaml],
    ['a shell script', '#!/bin/sh\necho hi', ValueLanguage.shell],
    ['a password', 'hunter2', ValueLanguage.plaintext],
    ['an LDAP password hash', '{SSHA}W6ph5Mm5Pz8GgiULbPgzG37mj9g=', ValueLanguage.plaintext],
    ['a bracketed placeholder', '[redacted]', ValueLanguage.plaintext],
    ['an angle-bracketed placeholder', '<secret>', ValueLanguage.plaintext],
    ['a padded base64 token', 'dGVzdA==', ValueLanguage.plaintext],
    ['a connection string', 'Server=db;Password=x;', ValueLanguage.plaintext],
    ['a sentence with a colon', 'Note: this is a password', ValueLanguage.plaintext],
    ['a numeric token', '123456', ValueLanguage.plaintext],
    ['a URL', 'https://user:pass@example.com/path', ValueLanguage.plaintext],
    [
      'a PEM certificate',
      '-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----',
      ValueLanguage.plaintext,
    ],
    ['an empty value', '', ValueLanguage.plaintext],
  ])('detects %s from the content', (_description, value, expected) => {
    expect(detectValueLanguage('password', value)).toBe(expected);
  });

  it('prefers the key extension over the content', () => {
    expect(detectValueLanguage('config.yaml', '{"a": 1}')).toBe(ValueLanguage.yaml);
  });

  it('detects docker config JSON from its content', () => {
    expect(detectValueLanguage('.dockerconfigjson', '{"auths":{}}')).toBe(ValueLanguage.json);
  });
});

describe('value formatters', () => {
  it('pretty prints JSON without altering number precision', async () => {
    expect(await format(ValueLanguage.json, '{"id":12345678901234567890,"a":[1,2]}')).toBe(
      '{\n  "id": 12345678901234567890,\n  "a": [\n    1,\n    2\n  ]\n}',
    );
  });

  it('rejects invalid JSON', async () => {
    await expect(format(ValueLanguage.json, '{"a": }')).rejects.toThrow();
  });

  it('formats YAML and keeps comments and documents', async () => {
    expect(await format(ValueLanguage.yaml, 'a:   1\n# keep me\nb:\n    - x\n---\nc:    2\n')).toBe(
      'a: 1\n# keep me\nb:\n  - x\n---\nc: 2\n',
    );
  });

  it('rejects invalid YAML', async () => {
    await expect(format(ValueLanguage.yaml, 'a: [1\nb: 2')).rejects.toThrow();
  });

  it('indents XML', async () => {
    expect(
      await format(ValueLanguage.xml, '<settings><server id="a">host</server></settings>'),
    ).toBe('<settings>\n  <server id="a">host</server>\n</settings>');
  });

  it('rejects invalid XML', async () => {
    await expect(format(ValueLanguage.xml, '<a><b></a>')).rejects.toThrow();
  });

  it('normalizes INI key/value spacing and section breaks', async () => {
    expect(
      await format(
        ValueLanguage.ini,
        '[a]\nkey=value=x   \nother  =  y\n  continued=line\n\n\n[b]\n; note\nz=1\n\n',
      ),
    ).toBe('[a]\nkey = value=x\nother = y\n  continued=line\n\n[b]\n; note\nz = 1\n');
  });

  it('leaves INI lines that are not plain key/value pairs alone', async () => {
    const text = '[s]\nabcd==\nif $x == "a" then stop\nRewriteRule ^/old?id=1 /new';
    expect(await format(ValueLanguage.ini, text)).toBe(text);
  });

  it('does not change whether the value ends with a newline', async () => {
    expect(await format(ValueLanguage.yaml, 'a:   1')).toBe('a: 1');
    expect(await format(ValueLanguage.xml, '<a><b/></a>\n')).toBe('<a>\n  <b/>\n</a>\n');
  });

  it('only offers formatting for languages with a formatter', () => {
    expect(VALUE_LANGUAGES[ValueLanguage.toml].format).toBeUndefined();
    expect(VALUE_LANGUAGES[ValueLanguage.properties].format).toBeUndefined();
    expect(VALUE_LANGUAGES[ValueLanguage.plaintext].format).toBeUndefined();
  });
});
