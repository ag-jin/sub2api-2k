<template>
  <div class="plaza-catalog-table overflow-x-auto">
    <table class="w-full min-w-[760px] table-fixed border-collapse text-sm tabular-nums">
      <colgroup>
        <col class="w-[24%]" />
        <col class="w-[12%]" />
        <col class="w-[12%]" />
        <col class="w-[24%]" />
        <col class="w-[16%]" />
        <col class="w-[12%]" />
      </colgroup>
      <thead>
        <tr class="border-b border-gray-200 text-xs font-semibold uppercase tracking-wider text-gray-500 dark:border-dark-700 dark:text-dark-400">
          <th class="py-2.5 pl-5 pr-4 text-left font-semibold">{{ t('modelPlaza.table.model') }}</th>
          <th class="px-3 py-2.5 text-left font-semibold">{{ t('modelPlaza.table.protocol') }}</th>
          <th class="px-3 py-2.5 text-left font-semibold">{{ t('modelPlaza.table.billing') }}</th>
          <th class="px-3 py-2.5 text-left font-semibold">
            {{ t('modelPlaza.table.price') }}
            <span class="ml-1 normal-case font-normal text-gray-400 dark:text-dark-500">{{ t('modelPlaza.table.unitPerMillion') }}</span>
          </th>
          <th class="px-3 py-2.5 text-left font-semibold">{{ t('modelPlaza.table.cache') }}</th>
          <th class="py-2.5 pl-3 pr-5 text-center font-semibold">{{ t('modelPlaza.table.direct') }}</th>
        </tr>
      </thead>
      <tbody>
        <template v-for="m in displayModels" :key="m.id">
          <tr
            v-for="(p, idx) in m.protocols"
            :key="`${m.id}:${p.protocol}`"
            class="border-b border-gray-100 transition-colors last:border-b-0 hover:bg-gray-50/70 dark:border-dark-800 dark:hover:bg-dark-800/50"
          >
            <!-- 模型:展示名 + 模型标识,协议的协议行合并单元格 -->
            <td
              v-if="idx === 0"
              :rowspan="m.protocols.length"
              class="border-r border-gray-100 py-2.5 pl-5 pr-4 align-middle dark:border-dark-700/60"
            >
              <div class="font-medium text-gray-900 dark:text-white">{{ m.display_name }}</div>
              <div class="mt-0.5 font-mono text-xs text-gray-400 dark:text-dark-500">{{ m.id }}</div>
            </td>

            <!-- 协议 -->
            <td class="px-3 py-2.5 align-middle">
              <span
                class="inline-flex items-center rounded-md px-2 py-0.5 text-xs font-medium"
                :class="platformBadgeLightClass(p.protocol)"
              >
                {{ p.protocol }}
              </span>
            </td>

            <!-- 计费模式 -->
            <td class="px-3 py-2.5 align-middle">
              <span class="rounded-md bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-500 dark:bg-dark-700/70 dark:text-dark-300">
                {{ billingModeLabel(p) }}
              </span>
            </td>

            <!-- 价格:token 输入/输出(阶梯内联);按次/按图/按视频为单单位价 -->
            <td class="px-3 py-2.5 align-middle">
              <template v-if="p.billing_mode === BILLING_MODE_TOKEN">
                <div class="space-y-0.5 font-mono text-xs text-gray-800 dark:text-gray-200">
                  <div v-if="tokenIntervals(p).length">
                    <template v-for="iv in tokenIntervals(p)" :key="iv.min_tokens">
                      <div class="whitespace-nowrap leading-5">
                        <span class="mr-1 font-sans font-normal text-gray-400 dark:text-dark-500">{{ t('modelPlaza.table.input') }} {{ tierLabel(iv) }}</span>
                        {{ perMillion(iv.input_price) }}
                      </div>
                    </template>
                  </div>
                  <div v-else class="whitespace-nowrap leading-5">
                    <span class="mr-1 font-sans font-normal text-gray-400 dark:text-dark-500">{{ t('modelPlaza.table.input') }}</span>
                    {{ perMillion(p.pricing?.input_price) }}
                  </div>
                  <div v-if="tokenIntervals(p).length">
                    <template v-for="iv in tokenIntervals(p)" :key="iv.min_tokens">
                      <div class="whitespace-nowrap leading-5">
                        <span class="mr-1 font-sans font-normal text-gray-400 dark:text-dark-500">{{ t('modelPlaza.table.output') }} {{ tierLabel(iv) }}</span>
                        {{ perMillion(iv.output_price) }}
                      </div>
                    </template>
                  </div>
                  <div v-else class="whitespace-nowrap leading-5">
                    <span class="mr-1 font-sans font-normal text-gray-400 dark:text-dark-500">{{ t('modelPlaza.table.output') }}</span>
                    {{ perMillion(p.pricing?.output_price) }}
                  </div>
                </div>
              </template>
              <!-- 非 token 计费:阶梯芯片或单一按单位价 -->
              <template v-else>
                <div
                  v-if="requestIntervals(p).length"
                  class="flex flex-wrap items-center gap-1.5"
                >
                  <span
                    v-for="(iv, i) in requestIntervals(p)"
                    :key="i"
                    class="inline-flex items-center gap-1 rounded-md bg-gray-100 px-2 py-0.5 font-mono text-xs text-gray-800 dark:bg-dark-700/60 dark:text-gray-200"
                  >
                    <span class="font-sans text-gray-400 dark:text-dark-500">{{ tierLabel(iv) }}</span>
                    {{ requestPrice(iv.per_request_price)
                    }}<span class="font-sans text-gray-400 dark:text-dark-500">{{ perUnitSuffix(p) }}</span>
                  </span>
                </div>
                <template v-else-if="p.pricing?.per_request_price != null">
                  <span class="font-mono text-xs font-semibold text-gray-900 dark:text-gray-50">
                    {{ requestPrice(p.pricing?.per_request_price) }}
                  </span>
                  <span class="ml-1 text-xs text-gray-400 dark:text-dark-500">{{ perUnitSuffix(p) }}</span>
                </template>
                <span v-else class="text-gray-400 dark:text-dark-500">-</span>
              </template>
            </td>

            <!-- 缓存(仅 token;写/读),多档时每档一行与输入/输出列对齐 -->
            <td class="px-3 py-2.5 align-middle">
              <template v-if="p.billing_mode === BILLING_MODE_TOKEN && hasTierCachePricing(p)">
                <div
                  v-for="(iv, idx) in tokenIntervals(p)"
                  :key="idx"
                  class="whitespace-nowrap font-mono text-xs leading-5 text-gray-800 dark:text-gray-200"
                >
                  <template v-if="iv.cache_write_price != null || iv.cache_read_price != null">
                    <span class="font-sans font-normal text-gray-400 dark:text-dark-500">{{ t('modelPlaza.table.cacheWriteShort') }}</span>
                    {{ perMillion(iv.cache_write_price) }}
                    <span class="ml-1 font-sans font-normal text-gray-400 dark:text-dark-500">{{ t('modelPlaza.table.cacheReadShort') }}</span>
                    {{ perMillion(iv.cache_read_price) }}
                  </template>
                  <span v-else class="text-gray-400 dark:text-dark-500">-</span>
                </div>
              </template>
              <div
                v-else-if="p.billing_mode === BILLING_MODE_TOKEN && hasCachePricing(p)"
                class="space-y-0.5 font-mono text-xs text-gray-800 dark:text-gray-200"
              >
                <div>
                  <span class="mr-1 font-sans font-normal text-gray-400 dark:text-dark-500">{{ t('modelPlaza.table.cacheWrite') }}</span>
                  {{ perMillion(p.pricing?.cache_write_price) }}
                </div>
                <div>
                  <span class="mr-1 font-sans font-normal text-gray-400 dark:text-dark-500">{{ t('modelPlaza.table.cacheRead') }}</span>
                  {{ perMillion(p.pricing?.cache_read_price) }}
                </div>
              </div>
              <span v-else class="text-gray-400 dark:text-dark-500">-</span>
            </td>

            <!-- 直连 / 中转 -->
            <td class="py-2.5 pl-3 pr-5 text-center align-middle">
              <span
                v-if="p.direct"
                class="inline-flex items-center gap-1 text-xs font-medium text-teal-600 dark:text-teal-400"
              >
                <Icon name="checkCircle" size="xs" class="h-3.5 w-3.5" />
                {{ t('modelPlaza.table.direct') }}
              </span>
              <span v-else class="inline-flex items-center gap-1 text-xs text-gray-400 dark:text-dark-500">
                <Icon name="x" size="xs" class="h-3.5 w-3.5" />
                {{ t('modelPlaza.table.relay') }}
              </span>
            </td>
          </tr>
        </template>
      </tbody>
    </table>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { formatScaled } from '@/utils/pricing'
import { platformBadgeLightClass } from '@/utils/platformColors'
import {
  BILLING_MODE_TOKEN,
  BILLING_MODE_IMAGE,
  BILLING_MODE_PER_REQUEST,
  BILLING_MODE_VIDEO,
  type BillingMode
} from '@/constants/channel'
import Icon from '@/components/icons/Icon.vue'
import type { CatalogModel, CatalogPricingInterval, CatalogProtocol } from '@/api/modelPlaza'

const props = defineProps<{
  /** 某个套餐下的模型清单（已过滤出非空协议行）。 */
  models: CatalogModel[]
}>()

const { t } = useI18n()

/**
 * 展示顺序:
 * 1. token 计费的协议行排在前,按图/按次/按视频沉到末尾——单位量纲不同,混排无意义;
 * 2. 同计费组内按输出价从高到低,无输出价的排最后;
 * 3. 同价按展示名降序(新版本号在前,如 gpt-5.6 先于 gpt-5.5)。
 */
const displayModels = computed(() =>
  [...props.models]
    .filter((m) => m.protocols.length > 0)
    .sort((a, b) => {
      const ta = a.protocols.some((p) => p.billing_mode === BILLING_MODE_TOKEN)
      const tb = b.protocols.some((p) => p.billing_mode === BILLING_MODE_TOKEN)
      if (ta !== tb) return ta ? -1 : 1
      const pa = maxOutputPrice(a)
      const pb = maxOutputPrice(b)
      if (pa != null && pb != null && pa !== pb) return pb - pa
      if (pa != null && pb == null) return -1
      if (pa == null && pb != null) return 1
      return b.display_name.localeCompare(a.display_name)
    })
    .map((m) => ({
      ...m,
      protocols: [...m.protocols].sort((x, y) =>
        billingRank(x.billing_mode) - billingRank(y.billing_mode) ||
        (x.protocol.localeCompare(y.protocol))
      )
    }))
)

/** 模型下所有协议行的输出价中最高者(无价返回 null),与旧版按官方输出价排序口径一致。 */
function maxOutputPrice(m: CatalogModel): number | null {
  let max: number | null = null
  for (const p of m.protocols) {
    const v = p.pricing?.output_price
    if (v != null && (max == null || v > max)) max = v
  }
  return max
}

/** token 组排最前,其余按常量定义的顺序(图片 < 按次 < 视频)。 */
function billingRank(mode: BillingMode): number {
  if (mode === BILLING_MODE_TOKEN) return 0
  if (mode === BILLING_MODE_IMAGE) return 1
  if (mode === BILLING_MODE_PER_REQUEST) return 2
  return 3
}

function billingModeLabel(p: CatalogProtocol): string {
  if (p.billing_mode === BILLING_MODE_IMAGE) return t('modelPlaza.table.perImage')
  if (p.billing_mode === BILLING_MODE_PER_REQUEST) return t('modelPlaza.table.perRequest')
  if (p.billing_mode === BILLING_MODE_VIDEO) return t('modelPlaza.table.perVideo')
  return t('modelPlaza.table.billingToken')
}

/** 价格统一保底 2 位小数,更长的有效小数原样保留。 */
const MIN_DECIMALS = 2
const PER_MILLION = 1_000_000

/** 目录价格即展示价(无内部倍率折算),token 计费按 $/1M token 展示。 */
function perMillion(value: number | null | undefined): string {
  if (value == null) return '-'
  return formatScaled(value, PER_MILLION, MIN_DECIMALS)
}

/** 按次 / 按图片 / 按视频单价(不换算 1M)。 */
function requestPrice(value: number | null | undefined): string {
  if (value == null) return '-'
  return formatScaled(value, 1, MIN_DECIMALS)
}

/** 非 token 计费的单位后缀:按图片 → “/ 张”,按次 → “/ 次”,按视频 → “/ 条”。 */
function perUnitSuffix(p: CatalogProtocol): string {
  if (p.billing_mode === BILLING_MODE_IMAGE) return t('modelPlaza.table.perUnitImage')
  if (p.billing_mode === BILLING_MODE_VIDEO) return t('modelPlaza.table.perUnitVideo')
  return t('modelPlaza.table.perUnitRequest')
}

function hasCachePricing(p: CatalogProtocol): boolean {
  return p.pricing?.cache_write_price != null || p.pricing?.cache_read_price != null
}

/** 任一档带缓存价才按档渲染缓存列;否则沿用平价的写入/读取两行。 */
function hasTierCachePricing(p: CatalogProtocol): boolean {
  return tokenIntervals(p).some((iv) => iv.cache_write_price != null || iv.cache_read_price != null)
}

/** 上下文档位按下限升序展示(后端已升序,此处兜底)。 */
function sortByContext(intervals: CatalogPricingInterval[]): CatalogPricingInterval[] {
  return [...intervals].sort((a, b) => a.min_tokens - b.min_tokens)
}

/** token 模式的阶梯定价(内联进输入/输出列)。 */
function tokenIntervals(p: CatalogProtocol): CatalogPricingInterval[] {
  return sortByContext(p.pricing?.intervals ?? [])
}

/** 按次/按图模式的阶梯定价(仅保留配了按单位价的档位)。 */
function requestIntervals(p: CatalogProtocol): CatalogPricingInterval[] {
  return sortByContext(p.pricing?.intervals ?? []).filter((iv) => iv.per_request_price != null)
}

/** 档位标签:优先管理员配置的 tier_label,否则按 token 区间生成(≤200K / >200K / 200K–1M)。 */
function tierLabel(iv: CatalogPricingInterval): string {
  if (iv.tier_label) return iv.tier_label
  const { min_tokens: min, max_tokens: max } = iv
  if (max == null) return `>${formatTokenCount(min)}`
  if (min === 0) return `≤${formatTokenCount(max)}`
  return `${formatTokenCount(min)}–${formatTokenCount(max)}`
}

function formatTokenCount(n: number): string {
  if (n >= 1_000_000) return `${trimZero(n / 1_000_000)}M`
  if (n >= 1_000) return `${trimZero(n / 1_000)}K`
  return String(n)
}

function trimZero(n: number): string {
  return String(Math.round(n * 100) / 100)
}
</script>