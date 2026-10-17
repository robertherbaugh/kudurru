// The tree-shaken ECharts build the dashboard needs: pie and bar charts,
// tooltip and legend, canvas renderer. Imported dynamically by EChart so
// ECharts never lands in the initial bundle.
import * as echarts from 'echarts/core';
import { BarChart, PieChart } from 'echarts/charts';
import { GridComponent, LegendComponent, TooltipComponent } from 'echarts/components';
import { CanvasRenderer } from 'echarts/renderers';

echarts.use([PieChart, BarChart, GridComponent, LegendComponent, TooltipComponent, CanvasRenderer]);

export { echarts };
export type EChartsOption = echarts.EChartsCoreOption;
