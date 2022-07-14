import { css } from '@emotion/css';
import React, { FC, useCallback, useEffect, useRef, useState } from 'react';
import { useDispatch, useSelector } from 'react-redux';
import { useLocalStorage } from 'react-use';

import { GrafanaTheme, PanelData, SelectableValue } from '@grafana/data';
import { selectors } from '@grafana/e2e-selectors';
import { Button, CustomScrollbar, Field, FilterInput, RadioButtonGroup, useStyles } from '@grafana/ui';
import { LS_VISUALIZATION_SELECT_TAB_KEY } from 'app/core/constants';
import { PanelLibraryOptionsGroup } from 'app/features/library-panels/components/PanelLibraryOptionsGroup/PanelLibraryOptionsGroup';
import { VisualizationSuggestions } from 'app/features/panel/components/VizTypePicker/VisualizationSuggestions';
import { VizTypeChangeDetails } from 'app/features/panel/components/VizTypePicker/types';

import { VizTypePicker } from '../../../panel/components/VizTypePicker/VizTypePicker';
import { changePanelPlugin } from '../../../panel/state/actions';
import { PanelModel } from '../../state/PanelModel';
import { getPanelPluginWithFallback } from '../../state/selectors';

import { toggleVizPicker } from './state/reducers';
import { VisualizationSelectPaneTab } from './types';

interface Props {
  panel: PanelModel;
  data?: PanelData;
}

export const VisualizationSelectPane: FC<Props> = ({ panel, data }) => {
  const plugin = useSelector(getPanelPluginWithFallback(panel.type));
  const [searchQuery, setSearchQuery] = useState('');
  const [listMode, setListMode] = useLocalStorage(
    LS_VISUALIZATION_SELECT_TAB_KEY,
    VisualizationSelectPaneTab.Visualizations
  );

  const dispatch = useDispatch();
  const styles = useStyles(getStyles);
  const searchRef = useRef<HTMLInputElement | null>(null);

  const onVizChange = useCallback(
    (pluginChange: VizTypeChangeDetails) => {
      dispatch(changePanelPlugin({ panel: panel, ...pluginChange }));

      // close viz picker unless a mod key is pressed while clicking
      if (!pluginChange.withModKey) {
        dispatch(toggleVizPicker(false));
      }
    },
    [dispatch, panel]
  );

  // Give Search input focus when using radio button switch list mode
  useEffect(() => {
    if (searchRef.current) {
      searchRef.current.focus();
    }
  }, [listMode]);

  const onCloseVizPicker = () => {
    dispatch(toggleVizPicker(false));
  };

  if (!plugin) {
    return null;
  }

  const radioOptions: Array<SelectableValue<VisualizationSelectPaneTab>> = [
    { label: 'Visualizations', value: VisualizationSelectPaneTab.Visualizations },
    { label: 'Suggestions', value: VisualizationSelectPaneTab.Suggestions },
    {
      label: 'Library panels',
      value: VisualizationSelectPaneTab.LibraryPanels,
      description: 'Reusable panels you can share between multiple dashboards.',
    },
  ];

  return (
    <div className={styles.openWrapper}>
      <div className={styles.formBox}>
        <div className={styles.searchRow}>
          <FilterInput
            value={searchQuery}
            onChange={setSearchQuery}
            ref={searchRef}
            autoFocus={true}
            placeholder="Search for..."
          />
          <Button
            title="Close"
            variant="secondary"
            icon="angle-up"
            className={styles.closeButton}
            aria-label={selectors.components.PanelEditor.toggleVizPicker}
            onClick={onCloseVizPicker}
          />
        </div>
        <Field className={styles.customFieldMargin}>
          <RadioButtonGroup options={radioOptions} value={listMode} onChange={setListMode} fullWidth />
        </Field>
      </div>
      <div className={styles.scrollWrapper}>
        <CustomScrollbar autoHeightMin="100%">
          <div className={styles.scrollContent}>
            {listMode === VisualizationSelectPaneTab.Visualizations && (
              <VizTypePicker
                current={plugin.meta}
                onChange={onVizChange}
                searchQuery={searchQuery}
                data={data}
                onClose={() => {}}
              />
            )}
            {listMode === VisualizationSelectPaneTab.Suggestions && (
              <VisualizationSuggestions
                current={plugin.meta}
                onChange={onVizChange}
                searchQuery={searchQuery}
                panel={panel}
                data={data}
                onClose={() => {}}
              />
            )}
            {listMode === VisualizationSelectPaneTab.LibraryPanels && (
              <PanelLibraryOptionsGroup searchQuery={searchQuery} panel={panel} key="Panel Library" />
            )}
          </div>
        </CustomScrollbar>
      </div>
    </div>
  );
};

VisualizationSelectPane.displayName = 'VisualizationSelectPane';

const getStyles = (theme: GrafanaTheme) => {
  return {
    icon: css`
      color: ${theme.palette.gray33};
    `,
    wrapper: css`
      display: flex;
      flex-direction: column;
      flex: 1 1 0;
      height: 100%;
    `,
    vizButton: css`
      text-align: left;
    `,
    scrollWrapper: css`
      flex-grow: 1;
      min-height: 0;
    `,
    scrollContent: css`
      padding: ${theme.spacing.sm};
    `,
    openWrapper: css`
      display: flex;
      flex-direction: column;
      flex: 1 1 100%;
      height: 100%;
      background: ${theme.colors.bg1};
      border: 1px solid ${theme.colors.border1};
    `,
    searchRow: css`
      display: flex;
      margin-bottom: ${theme.spacing.sm};
    `,
    closeButton: css`
      margin-left: ${theme.spacing.sm};
    `,
    customFieldMargin: css`
      margin-bottom: ${theme.spacing.sm};
    `,
    formBox: css`
      padding: ${theme.spacing.sm};
      padding-bottom: 0;
    `,
  };
};
