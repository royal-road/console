import { AsyncComponent } from '../../utils/async';
import type { DroppableFileInputProps } from '../../utils/file-input';

// TODO :Create a shared LazyDroppableFileInput component.
export const DroppableFileInput = (props: DroppableFileInputProps) => (
  <AsyncComponent
    loader={() => import('../../utils/file-input').then((c) => c.DroppableFileInput)}
    {...props}
  />
);
