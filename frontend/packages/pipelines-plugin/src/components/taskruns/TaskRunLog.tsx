import * as React from 'react';
import { useTranslation } from 'react-i18next';
import { Firehose } from '@console/internal/components/utils';
import { StatusBox } from '@console/internal/components/utils/status-box';
import { PodModel } from '@console/internal/models';
import LogsWrapperComponent from '../pipelineruns/logs/LogsWrapperComponent';
import { TaskRunKind } from '../../utils/pipeline-augment';
import './TaskRunLog.scss';

export type TaskRunLogProps = {
  obj: TaskRunKind;
};

const TaskRunLog: React.FC<TaskRunLogProps> = ({ obj }) => {
  const { t } = useTranslation();
  if (obj?.status?.podName) {
    const podResources = [
      {
        kind: PodModel.kind,
        isList: false,
        prop: `obj`,
        namespace: obj.metadata.namespace,
        name: obj.status.podName,
      },
    ];
    return (
      <div className="odc-task-run-log">
        <Firehose resources={podResources}>
          <LogsWrapperComponent
            taskName={obj.metadata.name}
            downloadAllLabel={t('pipelines-plugin~Download all Task Run logs')}
          />
        </Firehose>
      </div>
    );
  }
  return (
    <StatusBox
      loadError={t('pipelines-plugin~Pod not found')}
      label={t('pipelines-plugin~Task Run log')}
    />
  );
};

export default TaskRunLog;
