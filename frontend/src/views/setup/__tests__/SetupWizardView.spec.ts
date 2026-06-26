import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import SetupWizardView from '@/views/setup/SetupWizardView.vue'

const { testDatabaseMock, testRedisMock, installMock } = vi.hoisted(() => ({
  testDatabaseMock: vi.fn(),
  testRedisMock: vi.fn(),
  installMock: vi.fn(),
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key,
    locale: { value: 'en' },
  }),
}))

vi.mock('@/api/setup', async () => {
  const actual = await vi.importActual<typeof import('@/api/setup')>('@/api/setup')
  return {
    ...actual,
    testDatabase: (...args: any[]) => testDatabaseMock(...args),
    testRedis: (...args: any[]) => testRedisMock(...args),
    install: (...args: any[]) => installMock(...args),
  }
})

function findButtonByText(wrapper: ReturnType<typeof mount>, text: string) {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes(text))
  if (!button) {
    throw new Error(`Could not find button with text: ${text}`)
  }
  return button
}

describe('SetupWizardView', () => {
  beforeEach(() => {
    testDatabaseMock.mockReset()
    testRedisMock.mockReset()
    installMock.mockReset()

    testDatabaseMock.mockResolvedValue(undefined)
    testRedisMock.mockResolvedValue(undefined)
    installMock.mockResolvedValue({ message: 'ok', restart: true })
  })

  it('sends bootstrap SSH settings in the install payload', async () => {
    const wrapper = mount(SetupWizardView, {
      global: {
        stubs: {
          Select: true,
          Toggle: true,
          Icon: true,
          transition: false,
        },
      },
    })

    await flushPromises()

    await findButtonByText(wrapper, 'setup.status.testConnection').trigger('click')
    await flushPromises()
    await findButtonByText(wrapper, 'common.next').trigger('click')
    await flushPromises()

    await findButtonByText(wrapper, 'setup.status.testConnection').trigger('click')
    await flushPromises()
    await findButtonByText(wrapper, 'common.next').trigger('click')
    await flushPromises()

    await wrapper.find('input[type="email"]').setValue('admin@example.com')
    const passwordInputs = wrapper.findAll('input[type="password"]')
    await passwordInputs[0].setValue('secret123')
    await passwordInputs[1].setValue('secret123')
    await findButtonByText(wrapper, 'common.next').trigger('click')
    await flushPromises()

    await wrapper.find('[data-testid="bootstrap-ssh-host"]').setValue('child.example.internal')
    await wrapper.find('[data-testid="bootstrap-ssh-port"]').setValue('2222')
    await wrapper.find('[data-testid="bootstrap-ssh-user"]').setValue('root')
    await wrapper.find('[data-testid="bootstrap-ssh-deployment-dir"]').setValue('/opt/sub2api')
    await wrapper.find('[data-testid="bootstrap-ssh-bootstrap-only"]').setValue(true)

    await findButtonByText(wrapper, 'common.next').trigger('click')
    await flushPromises()

    await findButtonByText(wrapper, 'setup.status.completeInstallation').trigger('click')
    await flushPromises()

    expect(installMock).toHaveBeenCalledTimes(1)
    expect(installMock).toHaveBeenCalledWith(
      expect.objectContaining({
        bootstrap_ssh: {
          host: 'child.example.internal',
          port: 2222,
          user: 'root',
          deployment_dir: '/opt/sub2api',
          bootstrap_only: true,
        },
      })
    )
  })
})
