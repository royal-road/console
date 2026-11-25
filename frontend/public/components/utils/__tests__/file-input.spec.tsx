import { render, screen } from '@testing-library/react';
import { DroppableFileInput } from '../file-input';

jest.mock('@console/shared/src/components/editor/value-editor/ValueCodeEditor', () => ({
  __esModule: true,
  default: ({ fileName, value }: { fileName: string; value: string }) => (
    <div data-test="value-code-editor">{`${fileName}: ${value}`}</div>
  ),
}));

const mockMatchMedia = (matches: boolean) => {
  window.matchMedia = jest.fn().mockReturnValue({ matches });
};

const renderInput = (codeEditor?: { fileName: string }) =>
  render(
    <DroppableFileInput
      id="value"
      label="Value"
      filenamePlaceholder="Drop a file"
      inputFileData='{"a": 1}'
      onChange={jest.fn()}
      codeEditor={codeEditor}
    />,
  );

describe('DroppableFileInput', () => {
  const originalMatchMedia = window.matchMedia;
  afterEach(() => {
    window.matchMedia = originalMatchMedia;
  });

  it('uses the code editor on desktop when asked to', async () => {
    mockMatchMedia(false);
    renderInput({ fileName: 'config.json' });
    expect(await screen.findByTestId('value-code-editor')).toHaveTextContent(
      'config.json: {"a": 1}',
    );
    expect(screen.queryByTestId('file-input-textarea')).not.toBeInTheDocument();
    expect(window.matchMedia).toHaveBeenCalledWith(
      '(max-width: 767px), (hover: none) and (pointer: coarse)',
    );
  });

  it('keeps the textarea on mobile and touch-first devices', () => {
    mockMatchMedia(true);
    renderInput({ fileName: 'config.json' });
    expect(screen.getByTestId('file-input-textarea')).toHaveValue('{"a": 1}');
    expect(screen.queryByTestId('value-code-editor')).not.toBeInTheDocument();
  });

  it('keeps the textarea when no code editor is requested', () => {
    mockMatchMedia(false);
    renderInput();
    expect(screen.getByTestId('file-input-textarea')).toBeInTheDocument();
    expect(screen.queryByTestId('value-code-editor')).not.toBeInTheDocument();
  });
});
