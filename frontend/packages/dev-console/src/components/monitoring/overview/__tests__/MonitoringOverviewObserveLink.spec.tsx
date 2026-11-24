import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import MonitoringOverview from '../MonitoringOverview';
import { mockPodEvents, mockResourceEvents, mockPods } from './mockData';

jest.mock('../MonitoringMetrics', () => ({
  __esModule: true,
  default: () => null,
}));

jest.mock('../MonitoringOverviewEvents', () => ({
  __esModule: true,
  default: () => null,
}));

describe('MonitoringOverview metrics link', () => {
  it.each([
    ['Deployment', 'apps/v1', 'deployments'],
    ['StatefulSet', 'apps/v1', 'statefulsets'],
    ['DaemonSet', 'apps/v1', 'daemonsets'],
  ])('links a %s to its Observe tab', (kind, apiVersion, plural) => {
    render(
      <MemoryRouter>
        <MonitoringOverview
          resource={{
            apiVersion,
            kind,
            metadata: { name: 'workload-name', namespace: 'test' },
          }}
          pods={mockPods}
          resourceEvents={mockResourceEvents}
          monitoringAlerts={[]}
          {...mockPodEvents}
        />
      </MemoryRouter>,
    );

    expect(screen.getByTestId('observe-dashboards-link')).toHaveAttribute(
      'href',
      `/k8s/ns/test/${plural}/workload-name/observe`,
    );
  });
});
