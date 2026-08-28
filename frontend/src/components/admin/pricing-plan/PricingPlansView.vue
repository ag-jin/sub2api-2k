<template>
  <AppLayout>
    <div class="w-full min-w-0 space-y-6 pb-8">
      <header
        class="page-header mb-0 rounded-3xl bg-white p-5 shadow-sm ring-1 ring-gray-900/5 dark:bg-dark-800 dark:ring-dark-700 sm:p-6"
      >
        <h1 class="page-title flex items-center gap-2 text-xl font-black text-gray-900 dark:text-white">
          <span class="inline-flex h-8 w-8 items-center justify-center rounded-xl bg-blue-50 text-blue-500 dark:bg-blue-900/30 dark:text-blue-400">
            <Icon name="badge" size="sm" />
          </span>
          {{ t('admin.pricingPlans.title') }}
        </h1>
        <p class="page-description mt-1.5 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.pricingPlans.description') }}
        </p>
        <div class="mt-4 flex flex-wrap items-center gap-3 border-t border-gray-100 pt-4 dark:border-dark-700">
          <label class="flex cursor-pointer items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
            <Toggle v-model="includeDisabled" data-testid="include-disabled" />
            {{ t('admin.pricingPlans.includeDisabled') }}
          </label>
          <div class="ml-auto flex items-center gap-2">
            <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" data-testid="refresh-plans" @click="reload">
              <Icon name="refresh" size="sm" />
              {{ t('admin.pricingPlans.refresh') }}
            </button>
            <button type="button" class="btn btn-primary btn-sm" data-testid="create-plan" @click="openCreateDialog">
              <Icon name="plus" size="sm" />
              {{ t('admin.pricingPlans.createButton') }}
            </button>
          </div>
        </div>
      </header>

      <TablePageLayout>
        <template #table>
          <DataTable :columns="columns" :data="plans" :loading="loading">
            <template #cell-name="{ row, value }">
              <span class="font-medium text-gray-900 dark:text-white" :data-testid="`plan-name-${row.id}`">{{ value }}</span>
            </template>
            <template #cell-title="{ value }">
              <span class="text-sm text-gray-600 dark:text-gray-300">{{ value || '—' }}</span>
            </template>
            <template #cell-status="{ value }">
              <StatusBadge
                :status="value"
                :label="value === 'active' ? t('admin.pricingPlans.status.active') : t('admin.pricingPlans.status.disabled')"
              />
            </template>
            <template #cell-is_public="{ value }">
              <span class="flex justify-center">
                <Icon v-if="value" name="checkCircle" size="sm" class="text-green-500" />
                <Icon v-else name="xCircle" size="sm" class="text-gray-300 dark:text-dark-600" />
              </span>
            </template>
            <template #cell-sort_order="{ value }">
              <span class="text-sm text-gray-900 dark:text-gray-100">{{ value }}</span>
            </template>
            <template #cell-updated_at="{ value }">
              <span class="text-sm text-gray-500 dark:text-gray-400">{{ formatDateTime(value) }}</span>
            </template>
            <template #cell-actions="{ row }">
              <div class="flex items-center justify-end gap-1">
                <button type="button" class="btn btn-ghost btn-sm" data-testid="edit-plan" :title="t('admin.pricingPlans.edit')" @click="openEditDialog(row)">
                  <Icon name="edit" size="sm" />
                </button>
                <button type="button" class="btn btn-ghost btn-sm text-red-500 hover:bg-red-50 dark:hover:bg-red-900/20" data-testid="delete-plan" :title="t('admin.pricingPlans.delete')" @click="handleDelete(row)">
                  <Icon name="trash" size="sm" />
                </button>
              </div>
            </template>
            <template #empty>
              <EmptyState
                :title="t('admin.pricingPlans.emptyTitle')"
                :description="t('admin.pricingPlans.emptyDescription')"
                :action-text="t('admin.pricingPlans.createButton')"
                @action="openCreateDialog"
              />
            </template>
          </DataTable>
        </template>
      </TablePageLayout>
    </div>

    <PricingPlanDialog
      :show="showDialog"
      :plan-id="editingId"
      @close="closeDialog"
      @saved="reload"
    />

    <ConfirmDialog
      :show="showDeleteDialog"
      :title="t('common.delete')"
      :message="deleteConfirmMessage"
      :confirm-text="t('common.delete')"
      :cancel-text="t('common.cancel')"
      :danger="true"
      @confirm="confirmDelete"
      @cancel="showDeleteDialog = false"
    />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatDateTime } from '@/utils/format'
import { adminAPI } from '@/api/admin'
import type { AdminPricingPlan } from '@/api/admin/pricingPlans'
import type { Column } from '@/components/common/types'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import StatusBadge from '@/components/common/StatusBadge.vue'
import Toggle from '@/components/common/Toggle.vue'
import Icon from '@/components/icons/Icon.vue'
import PricingPlanDialog from '@/components/admin/pricing-plan/PricingPlanDialog.vue'

const { t } = useI18n()
const appStore = useAppStore()

const plans = ref<AdminPricingPlan[]>([])
const loading = ref(false)
const includeDisabled = ref(false)

const showDialog = ref(false)
/** null = 新建；非 null = 编辑（按 id 拉取全量详情） */
const editingId = ref<number | null>(null)

const showDeleteDialog = ref(false)
const deleting = ref<AdminPricingPlan | null>(null)

const columns = computed<Column[]>(() => [
  { key: 'name', label: t('admin.pricingPlans.columns.name'), sortable: false },
  { key: 'title', label: t('admin.pricingPlans.columns.title'), sortable: false },
  { key: 'status', label: t('admin.pricingPlans.columns.status'), sortable: false },
  { key: 'is_public', label: t('admin.pricingPlans.columns.isPublic'), sortable: false },
  { key: 'sort_order', label: t('admin.pricingPlans.columns.sortOrder'), sortable: false },
  { key: 'updated_at', label: t('admin.pricingPlans.columns.updatedAt'), sortable: false },
  { key: 'actions', label: t('admin.pricingPlans.columns.actions'), sortable: false }
])

const deleteConfirmMessage = computed(() => {
  const name = deleting.value?.name || ''
  return t('admin.pricingPlans.deleteConfirm', { name })
})

async function reload() {
  loading.value = true
  try {
    plans.value = await adminAPI.pricingPlans.list(includeDisabled.value)
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.pricingPlans.loadError')))
  } finally {
    loading.value = false
  }
}

function openCreateDialog() {
  editingId.value = null
  showDialog.value = true
}

function openEditDialog(row: AdminPricingPlan) {
  editingId.value = row.id
  showDialog.value = true
}

function closeDialog() {
  showDialog.value = false
  editingId.value = null
}

function handleDelete(row: AdminPricingPlan) {
  deleting.value = row
  showDeleteDialog.value = true
}

async function confirmDelete() {
  if (!deleting.value) return
  try {
    await adminAPI.pricingPlans.delete(deleting.value.id)
    appStore.showSuccess(t('admin.pricingPlans.deleteSuccess'))
    showDeleteDialog.value = false
    deleting.value = null
    reload()
  } catch (err: unknown) {
    appStore.showError(extractApiErrorMessage(err, t('admin.pricingPlans.deleteFailed')))
  }
}

onMounted(() => {
  void reload()
})

// 切换「显示停用」后自动刷新列表
watch(includeDisabled, () => {
  void reload()
})
</script>