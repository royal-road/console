import { safeLoad } from 'js-yaml';

export enum ValueLanguage {
  json = 'json',
  yaml = 'yaml',
  xml = 'xml',
  ini = 'ini',
  toml = 'toml',
  properties = 'properties',
  shell = 'shell',
  plaintext = 'plaintext',
}

/** Rejects when the text cannot be parsed as the formatter's language. */
type Formatter = (text: string) => Promise<string>;

const formatJSON: Formatter = async (text) => {
  const { applyEdits, format, parse } = await import('jsonc-parser');
  const errors = [];
  parse(text, errors, { allowTrailingComma: true });
  if (errors.length) {
    throw new Error('Invalid JSON');
  }
  // jsonc-parser rewrites whitespace only, so big numbers and comments survive untouched.
  return applyEdits(text, format(text, undefined, { insertSpaces: true, tabSize: 2, eol: '\n' }));
};

const formatYAML: Formatter = async (text) => {
  const { parseAllDocuments } = await import('yaml');
  const documents = [...parseAllDocuments(text)];
  const error = documents.flatMap((document) => document.errors)[0];
  if (error) {
    throw error;
  }
  // lineWidth 0 keeps long scalars on one line instead of folding them.
  return documents.map((document) => document.toString({ lineWidth: 0 })).join('');
};

const formatXML: Formatter = async (text) => {
  const xmlFormatter = await import('xml-formatter');
  // The CommonJS build assigns the function to module.exports, so `default` can be missing.
  const formatXml =
    xmlFormatter.default ?? (xmlFormatter as unknown as typeof xmlFormatter.default);
  return formatXml(text, {
    indentation: '  ',
    collapseContent: true,
    lineSeparator: '\n',
    strictMode: true,
  });
};

/** A plain `key = value` line; `==` is left alone so expressions are not rewritten. */
const INI_KEY_VALUE = /^([\w.-][\w. -]*?)\s*=(?!=)\s*(.*)$/;

/**
 * Normalizes `key = value` spacing, trailing whitespace, and blank lines around sections.
 * Indented lines are left alone because some parsers treat them as continuations of the
 * previous value, and so is anything that is not clearly a key/value pair.
 */
const formatINI: Formatter = async (text) => {
  const lines: string[] = [];
  const pushBlankLine = () => {
    if (lines.length && lines[lines.length - 1] !== '') {
      lines.push('');
    }
  };
  text.split(/\r?\n/).forEach((rawLine) => {
    const line = rawLine.trimEnd();
    if (!line) {
      pushBlankLine();
      return;
    }
    if (line.startsWith('[')) {
      pushBlankLine();
    }
    const keyValue = /^\s/.test(line) ? null : INI_KEY_VALUE.exec(line);
    lines.push(keyValue ? `${keyValue[1]} = ${keyValue[2]}` : line);
  });
  while (lines[lines.length - 1] === '') {
    lines.pop();
  }
  return lines.join('\n');
};

/** Formatting should not change whether the value ends with a newline. */
const keepFinalNewline =
  (format: Formatter): Formatter =>
  async (text) => {
    const formatted = (await format(text)).replace(/\n+$/, '');
    return /\n$/.test(text) ? `${formatted}\n` : formatted;
  };

type LanguageDefinition = {
  /** Monaco language that provides the syntax highlighting. */
  monacoLanguage: string;
  format?: Formatter;
};

export const VALUE_LANGUAGES: Record<ValueLanguage, LanguageDefinition> = {
  [ValueLanguage.json]: { monacoLanguage: 'json', format: keepFinalNewline(formatJSON) },
  [ValueLanguage.yaml]: { monacoLanguage: 'yaml', format: keepFinalNewline(formatYAML) },
  [ValueLanguage.xml]: { monacoLanguage: 'xml', format: keepFinalNewline(formatXML) },
  [ValueLanguage.ini]: { monacoLanguage: 'ini', format: keepFinalNewline(formatINI) },
  // Monaco has no TOML or properties grammar; the INI one covers sections, keys and comments.
  [ValueLanguage.toml]: { monacoLanguage: 'ini' },
  [ValueLanguage.properties]: { monacoLanguage: 'ini' },
  [ValueLanguage.shell]: { monacoLanguage: 'shell' },
  [ValueLanguage.plaintext]: { monacoLanguage: 'plaintext' },
};

const EXTENSION_LANGUAGES: Record<string, ValueLanguage> = {
  json: ValueLanguage.json,
  yaml: ValueLanguage.yaml,
  yml: ValueLanguage.yaml,
  xml: ValueLanguage.xml,
  xsd: ValueLanguage.xml,
  xsl: ValueLanguage.xml,
  xslt: ValueLanguage.xml,
  plist: ValueLanguage.xml,
  svg: ValueLanguage.xml,
  ini: ValueLanguage.ini,
  cnf: ValueLanguage.ini,
  toml: ValueLanguage.toml,
  properties: ValueLanguage.properties,
  env: ValueLanguage.properties,
  sh: ValueLanguage.shell,
  bash: ValueLanguage.shell,
};

/** Content sniffing is skipped above this size to keep typing responsive. */
const MAX_SNIFF_LENGTH = 256 * 1024;

const SECTION_HEADER = /^\[[^[\]=]+\]$/;
const TOML_ONLY_SYNTAX = /^\[\[[^[\]=]+\]\]$|"""|'''/m;
const PROPERTY_LINE = /^(export\s+)?[\w.-]+\s*=/;
/** `<?xml ...?>`, a self-closing element, or an element closed by its matching tag. */
const XML_DOCUMENT = /^(<\?xml[\s\S]*|<[\w:.-]+[^>]*\/>|<([\w:.-]+)[^>]*>[\s\S]*<\/\2>)$/;

const isComment = (line: string) => /^[#;]/.test(line);

const parsesAsJSON = (text: string) => {
  try {
    JSON.parse(text);
    return true;
  } catch {
    return false;
  }
};

const parsesAsYAMLCollection = (text: string) => {
  try {
    const parsed = safeLoad(text);
    return parsed !== null && typeof parsed === 'object';
  } catch {
    return false;
  }
};

const detectFromKey = (key: string): ValueLanguage | undefined =>
  key.includes('.') ? EXTENSION_LANGUAGES[key.split('.').pop().toLowerCase()] : undefined;

const detectFromContent = (value: string): ValueLanguage => {
  const text = value.trim();
  if (!text || text.length > MAX_SNIFF_LENGTH || text.startsWith('-----BEGIN ')) {
    return ValueLanguage.plaintext;
  }
  if (text.startsWith('#!')) {
    return ValueLanguage.shell;
  }

  const lines = text
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter((line) => line && !isComment(line));

  // Passwords and tokens are single lines that often happen to start with `{`, `<` or `[`,
  // or contain `=` or `:`, so a single line is only structured when it fully parses.
  if (lines.length <= 1) {
    if (/^[[{]/.test(text) && parsesAsJSON(text)) {
      return ValueLanguage.json;
    }
    return XML_DOCUMENT.test(text) ? ValueLanguage.xml : ValueLanguage.plaintext;
  }

  if (text.startsWith('{')) {
    return ValueLanguage.json;
  }
  if (text.startsWith('<')) {
    return ValueLanguage.xml;
  }
  if (TOML_ONLY_SYNTAX.test(text)) {
    return ValueLanguage.toml;
  }
  if (text.startsWith('[') && (parsesAsJSON(text) || !SECTION_HEADER.test(lines[0]))) {
    return ValueLanguage.json;
  }
  if (lines.some((line) => SECTION_HEADER.test(line))) {
    return ValueLanguage.ini;
  }
  if (text.startsWith('---')) {
    return ValueLanguage.yaml;
  }
  if (lines.every((line) => PROPERTY_LINE.test(line))) {
    return ValueLanguage.properties;
  }
  return parsesAsYAMLCollection(text) ? ValueLanguage.yaml : ValueLanguage.plaintext;
};

/** Guesses the language of a value from the key's file extension, then its content. */
export const detectValueLanguage = (key: string, value: string): ValueLanguage =>
  detectFromKey(key) ?? detectFromContent(value);
