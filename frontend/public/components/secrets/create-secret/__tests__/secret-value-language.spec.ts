import {
  detectSecretValueLanguage,
  SECRET_VALUE_LANGUAGES,
  SecretValueLanguage,
} from '../secret-value-language';

const format = (language: SecretValueLanguage, text: string) =>
  SECRET_VALUE_LANGUAGES[language].format(text);

describe('detectSecretValueLanguage', () => {
  it.each([
    ['config.json', SecretValueLanguage.json],
    ['values.YAML', SecretValueLanguage.yaml],
    ['app.yml', SecretValueLanguage.yaml],
    ['settings.xml', SecretValueLanguage.xml],
    ['php.ini', SecretValueLanguage.ini],
    ['my.cnf', SecretValueLanguage.ini],
    ['config.toml', SecretValueLanguage.toml],
    ['app.properties', SecretValueLanguage.properties],
    ['.env', SecretValueLanguage.properties],
    ['entrypoint.sh', SecretValueLanguage.shell],
  ])('detects %s from the key extension', (key, expected) => {
    expect(detectSecretValueLanguage(key, 'anything')).toBe(expected);
  });

  it.each([
    ['a JSON object', '{"user": "admin", "password": "hunter2"}', SecretValueLanguage.json],
    ['a multi-line JSON object being typed', '{\n  "user": ', SecretValueLanguage.json],
    ['a JSON array', '[1, 2, 3]', SecretValueLanguage.json],
    ['a JSON array of strings', '["a", "b"]', SecretValueLanguage.json],
    ['an XML document', '<?xml version="1.0"?><settings/>', SecretValueLanguage.xml],
    ['a single XML element', '<user name="a">b</user>', SecretValueLanguage.xml],
    ['multi-line XML', '<settings>\n  <a/>\n</settings>', SecretValueLanguage.xml],
    ['an INI file', '[database]\nuser = admin\npassword = secret', SecretValueLanguage.ini],
    ['INI with leading comments', '; comment\n[server]\nport=80', SecretValueLanguage.ini],
    [
      'TOML array tables',
      '[[servers]]\nname = "a"\n[[servers]]\nname = "b"',
      SecretValueLanguage.toml,
    ],
    ['TOML multi-line strings', '[server]\nmotd = """\nhi\n"""', SecretValueLanguage.toml],
    [
      'a dotenv file',
      'DB_USER=admin\n# comment\nexport DB_PASS=secret',
      SecretValueLanguage.properties,
    ],
    ['a YAML mapping', 'user: admin\npassword: secret', SecretValueLanguage.yaml],
    ['a YAML list', '- one\n- two', SecretValueLanguage.yaml],
    ['a YAML stream', '---\na: 1\n---\nb: 2', SecretValueLanguage.yaml],
    ['a shell script', '#!/bin/sh\necho hi', SecretValueLanguage.shell],
    ['a password', 'hunter2', SecretValueLanguage.plaintext],
    ['an LDAP password hash', '{SSHA}W6ph5Mm5Pz8GgiULbPgzG37mj9g=', SecretValueLanguage.plaintext],
    ['a bracketed placeholder', '[redacted]', SecretValueLanguage.plaintext],
    ['an angle-bracketed placeholder', '<secret>', SecretValueLanguage.plaintext],
    ['a padded base64 token', 'dGVzdA==', SecretValueLanguage.plaintext],
    ['a connection string', 'Server=db;Password=x;', SecretValueLanguage.plaintext],
    ['a sentence with a colon', 'Note: this is a password', SecretValueLanguage.plaintext],
    ['a numeric token', '123456', SecretValueLanguage.plaintext],
    ['a URL', 'https://user:pass@example.com/path', SecretValueLanguage.plaintext],
    [
      'a PEM certificate',
      '-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----',
      SecretValueLanguage.plaintext,
    ],
    ['an empty value', '', SecretValueLanguage.plaintext],
  ])('detects %s from the content', (_description, value, expected) => {
    expect(detectSecretValueLanguage('password', value)).toBe(expected);
  });

  it('prefers the key extension over the content', () => {
    expect(detectSecretValueLanguage('config.yaml', '{"a": 1}')).toBe(SecretValueLanguage.yaml);
  });

  it('detects docker config JSON from its content', () => {
    expect(detectSecretValueLanguage('.dockerconfigjson', '{"auths":{}}')).toBe(
      SecretValueLanguage.json,
    );
  });
});

describe('secret value formatters', () => {
  it('pretty prints JSON without altering number precision', async () => {
    expect(await format(SecretValueLanguage.json, '{"id":12345678901234567890,"a":[1,2]}')).toBe(
      '{\n  "id": 12345678901234567890,\n  "a": [\n    1,\n    2\n  ]\n}',
    );
  });

  it('rejects invalid JSON', async () => {
    await expect(format(SecretValueLanguage.json, '{"a": }')).rejects.toThrow();
  });

  it('formats YAML and keeps comments and documents', async () => {
    expect(
      await format(SecretValueLanguage.yaml, 'a:   1\n# keep me\nb:\n    - x\n---\nc:    2\n'),
    ).toBe('a: 1\n# keep me\nb:\n  - x\n---\nc: 2\n');
  });

  it('rejects invalid YAML', async () => {
    await expect(format(SecretValueLanguage.yaml, 'a: [1\nb: 2')).rejects.toThrow();
  });

  it('indents XML', async () => {
    expect(
      await format(SecretValueLanguage.xml, '<settings><server id="a">host</server></settings>'),
    ).toBe('<settings>\n  <server id="a">host</server>\n</settings>');
  });

  it('rejects invalid XML', async () => {
    await expect(format(SecretValueLanguage.xml, '<a><b></a>')).rejects.toThrow();
  });

  it('normalizes INI key/value spacing and section breaks', async () => {
    expect(
      await format(
        SecretValueLanguage.ini,
        '[a]\nkey=value=x   \nother  =  y\n  continued=line\n\n\n[b]\n; note\nz=1\n\n',
      ),
    ).toBe('[a]\nkey = value=x\nother = y\n  continued=line\n\n[b]\n; note\nz = 1\n');
  });

  it('leaves INI lines that are not plain key/value pairs alone', async () => {
    const text = '[s]\nabcd==\nif $x == "a" then stop\nRewriteRule ^/old?id=1 /new';
    expect(await format(SecretValueLanguage.ini, text)).toBe(text);
  });

  it('does not change whether the value ends with a newline', async () => {
    expect(await format(SecretValueLanguage.yaml, 'a:   1')).toBe('a: 1');
    expect(await format(SecretValueLanguage.xml, '<a><b/></a>\n')).toBe('<a>\n  <b/>\n</a>\n');
  });

  it('only offers formatting for languages with a formatter', () => {
    expect(SECRET_VALUE_LANGUAGES[SecretValueLanguage.toml].format).toBeUndefined();
    expect(SECRET_VALUE_LANGUAGES[SecretValueLanguage.properties].format).toBeUndefined();
    expect(SECRET_VALUE_LANGUAGES[SecretValueLanguage.plaintext].format).toBeUndefined();
  });
});
