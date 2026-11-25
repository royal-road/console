import type { FC } from 'react';
import { useMemo, useRef, useState } from 'react';
import type { EditorDidMount, Language } from '@patternfly/react-code-editor';
import { Button, HelperText, HelperTextItem } from '@patternfly/react-core';
import type { SimpleSelectOption } from '@patternfly/react-templates';
import { SimpleSelect } from '@patternfly/react-templates';
import { useTranslation } from 'react-i18next';
import { BasicCodeEditor } from '@console/shared/src/components/editor/BasicCodeEditor';
import {
  detectSecretValueLanguage,
  SECRET_VALUE_LANGUAGES,
  SecretValueLanguage,
} from './secret-value-language';

const AUTO_DETECT = 'auto';

/** Editor height bounds, in pixels; between them the editor grows with its (wrapped) content. */
const MIN_HEIGHT = 76;
const MAX_HEIGHT = 570;

type SecretValueEditorProps = {
  /** The secret key, used to detect the language from its file extension. */
  secretKey: string;
  value: string;
  onChange: (value: string) => void;
  isReadOnly?: boolean;
  label: string;
};

const SecretValueEditor: FC<SecretValueEditorProps> = ({
  secretKey,
  value,
  onChange,
  isReadOnly,
  label,
}) => {
  const { t } = useTranslation('public');
  const [selection, setSelection] = useState<SecretValueLanguage | typeof AUTO_DETECT>(AUTO_DETECT);
  const [formatError, setFormatError] = useState('');
  const [height, setHeight] = useState(MIN_HEIGHT);
  const editorRef = useRef<Parameters<EditorDidMount>[0]>();

  const detectedLanguage = useMemo(
    () => detectSecretValueLanguage(secretKey, value),
    [secretKey, value],
  );
  const language = selection === AUTO_DETECT ? detectedLanguage : selection;
  const { monacoLanguage, format: formatter } = SECRET_VALUE_LANGUAGES[language];

  const languageLabels: Record<SecretValueLanguage, string> = {
    [SecretValueLanguage.json]: t('JSON'),
    [SecretValueLanguage.yaml]: t('YAML'),
    [SecretValueLanguage.xml]: t('XML'),
    [SecretValueLanguage.ini]: t('INI'),
    [SecretValueLanguage.toml]: t('TOML'),
    [SecretValueLanguage.properties]: t('Properties / .env'),
    [SecretValueLanguage.shell]: t('Shell script'),
    [SecretValueLanguage.plaintext]: t('Plain text'),
  };

  const languageOptions: SimpleSelectOption[] = [
    {
      value: AUTO_DETECT,
      content: t('Auto-detect ({{language}})', { language: languageLabels[detectedLanguage] }),
      selected: selection === AUTO_DETECT,
    },
    ...Object.values(SecretValueLanguage).map((lang) => ({
      value: lang,
      content: languageLabels[lang],
      selected: selection === lang,
    })),
  ];

  const format = async () => {
    const editor = editorRef.current;
    const model = editor?.getModel();
    if (!model || !formatter) {
      return;
    }
    const current = model.getValue();
    try {
      const formatted = await formatter(current);
      setFormatError('');
      if (formatted !== current && model.getValue() === current) {
        // executeEdits keeps the change on the undo stack, unlike setValue.
        editor.pushUndoStop();
        editor.executeEdits('format-secret-value', [
          { range: model.getFullModelRange(), text: formatted },
        ]);
        editor.pushUndoStop();
      }
    } catch (e) {
      setFormatError(
        t('Could not format the value as {{language}}: {{error}}', {
          language: languageLabels[language],
          error: e.message,
        }),
      );
    }
  };
  // The editor action is registered once on mount, so it calls through a ref.
  const formatRef = useRef(format);
  formatRef.current = format;

  const onEditorDidMount: EditorDidMount = (editor, monaco) => {
    editorRef.current = editor;
    const fitContent = () =>
      setHeight(Math.min(Math.max(editor.getContentHeight(), MIN_HEIGHT), MAX_HEIGHT));
    editor.onDidContentSizeChange(fitContent);
    fitContent();
    // Overrides Monaco's Format Document shortcut so it also covers XML and INI.
    editor.addAction({
      id: 'console.secret-value.format',
      label: t('Format value'),
      // eslint-disable-next-line no-bitwise
      keybindings: [monaco.KeyMod.Shift | monaco.KeyMod.Alt | monaco.KeyCode.KeyF],
      run: () => formatRef.current(),
    });
  };

  const handleChange = (newValue: string) => {
    setFormatError('');
    onChange(newValue);
  };

  return (
    // Grows to fill the file upload row the way PatternFly sizes the default textarea.
    <div className="pf-v6-u-flex-1" style={{ minWidth: 0 }} data-test="secret-value-editor">
      <BasicCodeEditor
        code={value}
        onChange={handleChange}
        language={monacoLanguage as Language}
        onEditorDidMount={onEditorDidMount}
        isReadOnly={isReadOnly}
        isCopyEnabled
        height={`${height}px`}
        options={{
          ariaLabel: label,
          scrollBeyondLastLine: false,
          tabSize: 2,
          wordWrap: 'on',
          // Keep completions from offering words found in other secret values on the page.
          wordBasedSuggestions: 'currentDocument',
        }}
        customControls={
          <>
            <SimpleSelect
              initialOptions={languageOptions}
              onSelect={(_e, selected) =>
                setSelection(selected as SecretValueLanguage | typeof AUTO_DETECT)
              }
              toggleProps={{ 'aria-label': t('Value language'), size: 'sm' }}
              toggleWidth="14em"
              data-test="secret-value-language"
            />
            <Button
              variant="link"
              size="sm"
              onClick={format}
              isDisabled={isReadOnly || !formatter}
              data-test="secret-value-format"
            >
              {t('Format')}
            </Button>
          </>
        }
      />
      {formatError && (
        <HelperText>
          <HelperTextItem variant="error" data-test="secret-value-format-error">
            {formatError}
          </HelperTextItem>
        </HelperText>
      )}
    </div>
  );
};

export default SecretValueEditor;
